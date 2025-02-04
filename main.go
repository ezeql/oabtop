package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Logger setup
var (
	logger *log.Logger
	mu     sync.Mutex
	cache  = "crypto_cache.json"
)

func init() {
	logFile, err := os.OpenFile("crypto_app.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Fatal("Failed to open log file:", err)
	}
	logger = log.New(logFile, "CRYPTO_APP: ", log.Ldate|log.Ltime|log.Lshortfile)
}

func logOperation(operation string, result interface{}, err error) {
	mu.Lock()
	defer mu.Unlock()
	if err != nil {
		logger.Printf("Operation: %s, Error: %v\n", operation, err)
	} else {
		logger.Printf("Operation: %s, Result: %v\n", operation, result)
	}
}

// CryptoProvider defines the interface for cryptocurrency data providers
type CryptoProvider interface {
	GetRecords(page, perPage int) ([]CryptoRecord, error)
}

// CryptoRecord with price changes
type CryptoRecord struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Symbol      string  `json:"symbol"`
	PriceUSD    float64 `json:"current_price"`
	Change1h    float64 `json:"price_change_percentage_1h_in_currency"`
	Change24h   float64 `json:"price_change_percentage_24h_in_currency"`
	Change7d    float64 `json:"price_change_percentage_7d_in_currency"`
	MarketCap   float64 `json:"market_cap"`
	Volume24h   float64 `json:"total_volume"`
	TotalSupply float64 `json:"total_supply"`
}

// CoingeckoProvider with retry mechanism
type CoingeckoProvider struct {
	client *http.Client
}

func NewCoingeckoProvider() *CoingeckoProvider {
	return &CoingeckoProvider{
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (p *CoingeckoProvider) GetRecords(page, perPage int) ([]CryptoRecord, error) {
	var coins []CryptoRecord

	// Check cache
	cacheInfo, err := os.Stat(cache)
	if err == nil {
		if time.Since(cacheInfo.ModTime()) < 30*time.Second {
			if data, err := os.ReadFile(cache); err == nil {
				if err := json.Unmarshal(data, &coins); err == nil {
					logOperation("Cache Hit", fmt.Sprintf("Fetched %d records from cache", len(coins)), nil)
					return coins, nil
				}
			}
		}
	}

	url := fmt.Sprintf("https://api.coingecko.com/api/v3/coins/markets?vs_currency=usd&order=market_cap_desc&per_page=%d&page=%d&sparkline=false&price_change_percentage=1h,24h,7d", perPage, page)
	var resp *http.Response

	// Retry with exponential backoff
	for retries, delay := 0, time.Second; retries < 5; retries, delay = retries+1, delay*2 {
		resp, err = p.client.Get(url)
		if err == nil && resp.StatusCode != http.StatusTooManyRequests {
			break
		}
		time.Sleep(delay)
	}

	if err != nil {
		logOperation("API Request", nil, err)
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logOperation("Read Response Body", nil, err)
		return nil, err
	}

	logOperation("JSON Response", string(body), nil)

	if err := json.Unmarshal(body, &coins); err != nil {
		logOperation("Unmarshal Response", nil, err)
		return nil, err
	}

	// Save to cache
	if data, err := json.Marshal(coins); err == nil {
		_ = os.WriteFile(cache, data, 0644)
	}

	logOperation("Fetch Records", fmt.Sprintf("Fetched %d records", len(coins)), nil)
	return coins, nil
}

var baseStyle = lipgloss.NewStyle().
	BorderStyle(lipgloss.NormalBorder()).
	BorderForeground(lipgloss.Color("240"))

// Define a consistent style for the fields at package level
var neutralStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

type model struct {
	table   table.Model
	spinner spinner.Model
	loading bool
	records []CryptoRecord
	page    int
	perPage int
	sortBy  string
	sortAsc bool
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, tea.EnterAltScreen)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			if m.table.Focused() {
				m.table.Blur()
			} else {
				m.table.Focus()
			}
		case "q", "ctrl+c":
			return m, tea.Quit
		case "enter":
			return m, tea.Printf("Selected: %s", m.table.SelectedRow()[1])
		case "right":
			if m.page*m.perPage < len(m.records) {
				m.page++
				m.updateTable()
			}
		case "left":
			if m.page > 1 {
				m.page--
				m.updateTable()
			}
		case "r", "n", "p", "1", "2", "7", "m", "a", "t":
			if m.sortBy == msg.String() {
				m.sortAsc = !m.sortAsc // Toggle sort order
			} else {
				m.sortBy = msg.String()
				m.sortAsc = true // Default to ascending when switching fields
			}
			m.sortRecords()
			m.updateTable()
		}
	case spinner.TickMsg:
		if m.loading {
			var cmds []tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			cmds = append(cmds, cmd)
			return m, tea.Batch(cmds...)
		}
	case tea.WindowSizeMsg:
		m.table.SetWidth(msg.Width)
		m.table.SetHeight(msg.Height - 2) // Use maximum available height
	}
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m model) View() string {
	if m.loading {
		return "\n" + m.spinner.View() + " Loading..."
	}
	return baseStyle.Render(m.table.View()) + "\n"
}

func (m *model) updateTable() {
	start := (m.page - 1) * m.perPage
	end := start + m.perPage
	if end > len(m.records) {
		end = len(m.records)
	}

	// Update column titles with sort indicators
	columns := []table.Column{
		{Title: "Rank", Width: 6},
		{Title: "Name", Width: 20},
		{Title: "Symbol", Width: 10},
		{Title: "Price (USD)", Width: 15},
		{Title: "1h", Width: 8},
		{Title: "24h", Width: 8},
		{Title: "7d", Width: 8},
		{Title: "Market Cap", Width: 15},
		{Title: "Volume (24h)", Width: 15},
		{Title: "Total Supply", Width: 15},
	}

	// Add arrow to the sorted column
	switch m.sortBy {
	case "r":
		columns[0].Title += getSortArrow(m.sortAsc)
	case "n":
		columns[1].Title += getSortArrow(m.sortAsc)
	case "p":
		columns[3].Title += getSortArrow(m.sortAsc)
	case "1":
		columns[4].Title += getSortArrow(m.sortAsc)
	case "2":
		columns[5].Title += getSortArrow(m.sortAsc)
	case "7":
		columns[6].Title += getSortArrow(m.sortAsc)
	case "m":
		columns[7].Title += getSortArrow(m.sortAsc)
	case "a":
		columns[8].Title += getSortArrow(m.sortAsc)
	case "t":
		columns[9].Title += getSortArrow(m.sortAsc)
	}

	m.table.SetColumns(columns)

	var rows []table.Row
	for i, record := range m.records[start:end] {
		rows = append(rows, table.Row{
			strconv.Itoa(i + 1 + start),                  // Rank
			record.Name,                                  // Name
			strings.ToUpper(record.Symbol),               // Symbol
			fmt.Sprintf("$%.2f", record.PriceUSD),        // Price
			colorizeChange(record.Change1h),              // 1h Change
			colorizeChange(record.Change24h),             // 24h Change
			colorizeChange(record.Change7d),              // 7d Change
			fmt.Sprintf("$%.2fM", record.MarketCap/1e6),  // Market Cap
			fmt.Sprintf("$%.2fM", record.Volume24h/1e6),  // Volume
			fmt.Sprintf("%.2fM", record.TotalSupply/1e6), // Total Supply
		})
	}
	m.table.SetRows(rows)
}

func (m *model) sortRecords() {
	switch m.sortBy {
	case "r":
		sort.Slice(m.records, func(i, j int) bool {
			if m.sortAsc {
				return m.records[i].MarketCap < m.records[j].MarketCap // Sort by market cap
			}
			return m.records[i].MarketCap > m.records[j].MarketCap
		})
	case "n":
		sort.Slice(m.records, func(i, j int) bool {
			if m.sortAsc {
				return strings.ToLower(m.records[i].Name)[0] < strings.ToLower(m.records[j].Name)[0]
			}
			return strings.ToLower(m.records[i].Name)[0] > strings.ToLower(m.records[j].Name)[0]
		})
	case "p":
		sort.Slice(m.records, func(i, j int) bool {
			if m.sortAsc {
				return m.records[i].PriceUSD < m.records[j].PriceUSD
			}
			return m.records[i].PriceUSD > m.records[j].PriceUSD
		})
	case "1":
		sort.Slice(m.records, func(i, j int) bool {
			if m.sortAsc {
				return m.records[i].Change1h < m.records[j].Change1h
			}
			return m.records[i].Change1h > m.records[j].Change1h
		})
	case "2":
		sort.Slice(m.records, func(i, j int) bool {
			if m.sortAsc {
				return m.records[i].Change24h < m.records[j].Change24h
			}
			return m.records[i].Change24h > m.records[j].Change24h
		})
	case "7":
		sort.Slice(m.records, func(i, j int) bool {
			if m.sortAsc {
				return m.records[i].Change7d < m.records[j].Change7d
			}
			return m.records[i].Change7d > m.records[j].Change7d
		})
	case "m":
		sort.Slice(m.records, func(i, j int) bool {
			if m.sortAsc {
				return m.records[i].MarketCap < m.records[j].MarketCap
			}
			return m.records[i].MarketCap > m.records[j].MarketCap
		})
	case "a":
		sort.Slice(m.records, func(i, j int) bool {
			if m.sortAsc {
				return m.records[i].Volume24h < m.records[j].Volume24h
			}
			return m.records[i].Volume24h > m.records[j].Volume24h
		})
	case "t":
		sort.Slice(m.records, func(i, j int) bool {
			if m.sortAsc {
				return m.records[i].TotalSupply < m.records[j].TotalSupply
			}
			return m.records[i].TotalSupply > m.records[j].TotalSupply
		})
	}
}

func main() {
	columns := []table.Column{
		{Title: "Rank", Width: 6},
		{Title: "Name", Width: 20},
		{Title: "Symbol", Width: 10},
		{Title: "Price (USD)", Width: 15},
		{Title: "1h", Width: 8},
		{Title: "24h", Width: 8},
		{Title: "7d", Width: 8},
		{Title: "Market Cap", Width: 15},
		{Title: "Volume (24h)", Width: 15},
		{Title: "Total Supply", Width: 15},
	}

	provider := NewCoingeckoProvider()
	records, err := provider.GetRecords(1, 50)
	if err != nil {
		logOperation("Main - Get Records", nil, err)
		log.Fatal("Error fetching data:", err)
	}

	var rows []table.Row
	for i, record := range records {
		rows = append(rows, table.Row{
			strconv.Itoa(i + 1),                          // Rank
			record.Name,                                  // Name
			strings.ToUpper(record.Symbol),               // Symbol
			fmt.Sprintf("$%.2f", record.PriceUSD),        // Price
			colorizeChange(record.Change1h),              // 1h Change
			colorizeChange(record.Change24h),             // 24h Change
			colorizeChange(record.Change7d),              // 7d Change
			fmt.Sprintf("$%.2fM", record.MarketCap/1e6),  // Market Cap
			fmt.Sprintf("$%.2fM", record.Volume24h/1e6),  // Volume
			fmt.Sprintf("%.2fM", record.TotalSupply/1e6), // Total Supply
		})
	}

	t := table.New(
		table.WithColumns(columns),
		table.WithRows(rows),
		table.WithFocused(true),
		table.WithHeight(10),
	)

	// Apply local styles
	s := table.DefaultStyles()
	s.Header = s.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("240")).
		BorderBottom(true).
		Bold(false)
	s.Selected = s.Selected.
		Foreground(lipgloss.Color("229")).
		Background(lipgloss.Color("57")).
		Bold(false)
	t.SetStyles(s)

	sp := spinner.New()
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("63"))

	m := model{table: t, spinner: sp, loading: false, records: records, page: 1, perPage: 50, sortBy: "r", sortAsc: true}
	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		logOperation("Run Program", nil, err)
		fmt.Println("Error running program:", err)
		os.Exit(1)
	}
}

func colorizeChange(change float64) string {
	if change < 0 {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render(fmt.Sprintf("%.2f%%", change))
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Render(fmt.Sprintf("%.2f%%", change))
}

// Helper function to get the sort arrow
func getSortArrow(asc bool) string {
	if asc {
		return " ↑"
	}
	return " ↓"
}
