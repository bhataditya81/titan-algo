package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"titan-algo/internal/discovery"
)

// SessionConfig holds user-selected trading configuration
type SessionConfig struct {
	Strategy        string   `json:"strategy"`
	Balance         float64  `json:"balance"`
	SymbolSelection string   `json:"symbol_selection"` // "manual", "top_n_volume", "highest_volume"
	TopNSymbols     int      `json:"top_n_symbols"`
	SelectedChains  []string `json:"selected_chains"`
	StopLossEnabled bool     `json:"stop_loss_enabled"`
	StopLossPercent float64  `json:"stop_loss_percent"`
	Timestamp       string   `json:"timestamp"`
}

// StrategyInfo describes available strategies
type StrategyInfo struct {
	Name        string
	Description string
	Icon        string
}

// Available strategies
var AvailableStrategies = []StrategyInfo{
	{"sniper", "Directional momentum trades", "🎯"},
	{"nine_twenty", "9:20 AM straddle strategy", "🕘"},
	{"short_straddle", "RSI-based premium selling", "📊"},
	{"iron_fly", "Hedged straddle with wings", "🦋"},
	{"momentum", "Trend-following strategy", "📈"},
	{"ema_crossover", "EMA crossover signals", "📉"},
	{"rsi_reversal", "Mean reversion trading", "🔄"},
}

// getSessionFilePath returns path to session file
func getSessionFilePath() string {
	homeDir, _ := os.UserHomeDir()
	titanDir := filepath.Join(homeDir, ".titan")
	os.MkdirAll(titanDir, 0755)
	return filepath.Join(titanDir, "session.json")
}

// QuickStart checks for saved session and offers to reuse
func QuickStart(reader *bufio.Reader) (*SessionConfig, bool) {
	sessionPath := getSessionFilePath()

	data, err := os.ReadFile(sessionPath)
	if err != nil {
		return nil, false
	}

	var session SessionConfig
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, false
	}

	// Display last session
	fmt.Println()
	printBox("🚀 TITAN ALGO TRADING")
	fmt.Println()
	fmt.Println("  Last Session Found:")
	fmt.Printf("  • Strategy: %s\n", session.Strategy)
	fmt.Printf("  • Balance: ₹%.2f\n", session.Balance)
	fmt.Printf("  • Mode: %s\n", session.SymbolSelection)
	if len(session.SelectedChains) > 0 {
		fmt.Printf("  • Symbols: %s\n", strings.Join(session.SelectedChains, ", "))
	}
	fmt.Println()
	fmt.Print("  Use these settings? [Y/n]: ")

	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(strings.ToLower(input))

	if input == "" || input == "y" || input == "yes" {
		return &session, true
	}

	return nil, false
}

// SaveSession persists session config for next startup
func SaveSession(config *SessionConfig) error {
	config.Timestamp = fmt.Sprintf("%v", os.Getenv(""))

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(getSessionFilePath(), data, 0644)
}

// RunInteractiveSetup runs the full interactive configuration
func RunInteractiveSetup(reader *bufio.Reader, chains []discovery.ChainInfo) (*SessionConfig, error) {
	return RunInteractiveSetupWithBalance(reader, chains, 0)
}

// RunInteractiveSetupWithBalance runs interactive configuration with a pre-entered balance
// If preEnteredBalance > 0, skips balance input step and uses the provided balance
func RunInteractiveSetupWithBalance(reader *bufio.Reader, chains []discovery.ChainInfo, preEnteredBalance float64) (*SessionConfig, error) {
	config := &SessionConfig{
		StopLossEnabled: true,
		StopLossPercent: 5.0,
	}

	// Step 1: Strategy Selection
	strategy, err := selectStrategy(reader)
	if err != nil {
		return nil, err
	}
	config.Strategy = strategy

	// Step 2: Balance Input (skip if pre-entered)
	var balance float64
	if preEnteredBalance > 0 {
		balance = preEnteredBalance
	} else {
		balance, err = InputBalance(reader)
		if err != nil {
			return nil, err
		}
	}
	config.Balance = balance

	// Step 3: Symbol Selection Mode
	mode, topN, err := selectSymbolMode(reader)
	if err != nil {
		return nil, err
	}
	config.SymbolSelection = mode
	config.TopNSymbols = topN

	// Step 4: Chain Selection (if manual mode)
	if mode == "manual" {
		selected, err := selectChains(reader, chains, topN)
		if err != nil {
			return nil, err
		}
		config.SelectedChains = selected
	} else if mode == "top_n_volume" {
		// Auto-select top N
		for i := 0; i < topN && i < len(chains); i++ {
			config.SelectedChains = append(config.SelectedChains, chains[i].Symbol)
		}
	} else {
		// highest_volume - just take top 1
		if len(chains) > 0 {
			config.SelectedChains = []string{chains[0].Symbol}
		}
	}

	// Step 5: Confirmation
	if !confirmConfig(reader, config) {
		return nil, fmt.Errorf("configuration cancelled by user")
	}

	return config, nil
}

// selectStrategy prompts user to select a trading strategy
func selectStrategy(reader *bufio.Reader) (string, error) {
	fmt.Println()
	printBox("📊 SELECT STRATEGY")
	fmt.Println()

	for i, s := range AvailableStrategies {
		fmt.Printf("  %d. %-15s %s %s\n", i+1, s.Name, s.Icon, s.Description)
	}

	fmt.Println()
	fmt.Print("  Enter choice [1-7]: ")

	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)

	choice, err := strconv.Atoi(input)
	if err != nil || choice < 1 || choice > len(AvailableStrategies) {
		return "sniper", nil // Default
	}

	return AvailableStrategies[choice-1].Name, nil
}

// InputBalance prompts user for session balance (exported for use before discovery)
func InputBalance(reader *bufio.Reader) (float64, error) {
	fmt.Println()
	printBox("💰 SESSION CONFIGURATION")
	fmt.Println()
	fmt.Print("  Enter Session Balance (INR): ₹")

	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)

	balance, err := strconv.ParseFloat(input, 64)
	if err != nil || balance <= 0 {
		return 10000.0, nil // Default
	}

	return balance, nil
}

// selectSymbolMode prompts user for symbol selection mode
func selectSymbolMode(reader *bufio.Reader) (string, int, error) {
	fmt.Println()
	fmt.Println("  Symbol Selection Mode:")
	fmt.Println("  1. Manual (select specific chains)")
	fmt.Println("  2. Top N Volume (auto-select highest volume)")
	fmt.Println("  3. Highest Volume Only (single chain)")
	fmt.Println()
	fmt.Print("  Enter choice [1-3]: ")

	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)

	choice, _ := strconv.Atoi(input)

	var mode string
	var topN int = 1

	switch choice {
	case 1:
		mode = "manual"
		fmt.Print("  How many chains to trade? [1-10]: ")
		nInput, _ := reader.ReadString('\n')
		topN, _ = strconv.Atoi(strings.TrimSpace(nInput))
		if topN < 1 || topN > 10 {
			topN = 3
		}
	case 2:
		mode = "top_n_volume"
		fmt.Print("  How many top chains? [1-10]: ")
		nInput, _ := reader.ReadString('\n')
		topN, _ = strconv.Atoi(strings.TrimSpace(nInput))
		if topN < 1 || topN > 10 {
			topN = 3
		}
	case 3:
		mode = "highest_volume"
		topN = 1
	default:
		mode = "manual"
		topN = 3
	}

	return mode, topN, nil
}

// selectChains displays chain table and allows selection
func selectChains(reader *bufio.Reader, chains []discovery.ChainInfo, maxSelect int) ([]string, error) {
	fmt.Println()
	printBox("📈 AFFORDABLE OPTION CHAINS BY VOLUME")
	fmt.Println()

	// Table header (include Lot and Cost)
	fmt.Println("  #   Symbol                    LTP      Lot   Cost        Volume")
	fmt.Println("  ─────────────────────────────────────────────────────────────────────")

	// Display chains
	displayCount := 10
	if len(chains) < displayCount {
		displayCount = len(chains)
	}

	for i := 0; i < displayCount; i++ {
		c := chains[i]
		volStr := formatVolume(c.Volume)
		costStr := formatCurrency(c.TradeCost)
		fmt.Printf("  %-3d %-25s ₹%-7.2f %-4d %-10s %s\n",
			i+1, c.Symbol, c.LTP, c.LotSize, costStr, volStr)
	}

	fmt.Println()
	fmt.Printf("  Enter chain numbers to trade (comma-separated) [e.g., 1,2,3]: ")

	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)

	// Parse selection
	var selected []string
	parts := strings.Split(input, ",")

	for _, p := range parts {
		p = strings.TrimSpace(p)
		idx, err := strconv.Atoi(p)
		if err != nil || idx < 1 || idx > displayCount {
			continue
		}
		if len(selected) >= maxSelect {
			break
		}
		selected = append(selected, chains[idx-1].Symbol)
	}

	if len(selected) == 0 && len(chains) > 0 {
		// Default to first chain
		selected = []string{chains[0].Symbol}
	}

	return selected, nil
}

// confirmConfig shows summary and asks for confirmation
func confirmConfig(reader *bufio.Reader, config *SessionConfig) bool {
	fmt.Println()
	printBox("✅ CONFIGURATION SUMMARY")
	fmt.Println()
	fmt.Printf("  Strategy:         %s\n", config.Strategy)
	fmt.Printf("  Balance:          ₹%.2f\n", config.Balance)
	fmt.Printf("  Selection Mode:   %s\n", config.SymbolSelection)
	if config.StopLossEnabled {
		fmt.Printf("  Stop-Loss:        %.1f%% (Enabled)\n", config.StopLossPercent)
	}
	fmt.Println()
	fmt.Println("  Selected Chains:")
	for _, c := range config.SelectedChains {
		fmt.Printf("  • %s\n", c)
	}
	fmt.Println()
	fmt.Print("  Start Trading? [Y/n]: ")

	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(strings.ToLower(input))

	return input == "" || input == "y" || input == "yes"
}

// Helper functions

func printBox(title string) {
	width := 60
	fmt.Println("╔" + strings.Repeat("═", width) + "╗")
	padding := (width - len(title)) / 2
	fmt.Printf("║%s%s%s║\n", strings.Repeat(" ", padding), title, strings.Repeat(" ", width-len(title)-padding))
	fmt.Println("╠" + strings.Repeat("═", width) + "╣")
}

func formatVolume(vol int64) string {
	if vol >= 10000000 {
		return fmt.Sprintf("%.1fCr", float64(vol)/10000000)
	}
	if vol >= 100000 {
		return fmt.Sprintf("%.1fL", float64(vol)/100000)
	}
	if vol >= 1000 {
		return fmt.Sprintf("%.1fK", float64(vol)/1000)
	}
	return fmt.Sprintf("%d", vol)
}

func formatCurrency(amount float64) string {
	if amount >= 100000 {
		return fmt.Sprintf("₹%.1fL", amount/100000)
	}
	if amount >= 1000 {
		return fmt.Sprintf("₹%.1fK", amount/1000)
	}
	return fmt.Sprintf("₹%.0f", amount)
}
