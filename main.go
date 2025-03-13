package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	_ "github.com/mattn/go-sqlite3"
)

type ClearinghouseRequest struct {
	Type string `json:"type"`
	User string `json:"user"`
}

type Position struct {
	Coin     string `json:"coin"`
	Szi      string `json:"szi"`
	Leverage struct {
		Type   string `json:"type"`
		Value  int    `json:"value"`
		RawUsd string `json:"rawUsd"`
	} `json:"leverage"`
	EntryPx        string `json:"entryPx"`
	PositionValue  string `json:"positionValue"`
	UnrealizedPnl  string `json:"unrealizedPnl"`
	ReturnOnEquity string `json:"returnOnEquity"`
	LiquidationPx  string `json:"liquidationPx"`
	MarginUsed     string `json:"marginUsed"`
	MaxLeverage    int    `json:"maxLeverage"`
	CumFunding     struct {
		AllTime     string `json:"allTime"`
		SinceOpen   string `json:"sinceOpen"`
		SinceChange string `json:"sinceChange"`
	} `json:"cumFunding"`
}

type AssetPosition struct {
	Type     string   `json:"type"`
	Position Position `json:"position"`
}

type Response struct {
	MarginSummary struct {
		AccountValue    string `json:"accountValue"`
		TotalNtlPos     string `json:"totalNtlPos"`
		TotalRawUsd     string `json:"totalRawUsd"`
		TotalMarginUsed string `json:"totalMarginUsed"`
	} `json:"marginSummary"`
	CrossMarginSummary struct {
		AccountValue    string `json:"accountValue"`
		TotalNtlPos     string `json:"totalNtlPos"`
		TotalRawUsd     string `json:"totalRawUsd"`
		TotalMarginUsed string `json:"totalMarginUsed"`
	} `json:"crossMarginSummary"`
	CrossMaintenanceMarginUsed string          `json:"crossMaintenanceMarginUsed"`
	Withdrawable               string          `json:"withdrawable"`
	AssetPositions             []AssetPosition `json:"assetPositions"`
	Time                       int64           `json:"time"`
}

type Config struct {
	TelegramToken   string `json:"telegramToken"`
	PollingInterval int    `json:"pollingInterval"`
	SuperAdminID    string `json:"superAdminID"`
}

type WalletConfig struct {
	Address string
	Name    string
	ChatID  string
}

type AccountState struct {
	LastPositions    map[string]Position
	LastAccountValue float64
}

const (
	ApiEndpoint = "https://api.hyperliquid.xyz/info"
	ConfigPath  = "config.json"
	DBPath      = "position-monitor.db"
)

var (
	accountStates   = make(map[string]*AccountState) // 键为 address
	wallets         = make(map[string]WalletConfig)  // 键为 chatID_address
	walletMutex     sync.Mutex
	bot             *tgbotapi.BotAPI
	db              *sql.DB
	authorizedUsers = make(map[string]bool)
	config          *Config
)

func main() {
	var err error
	db, err = initDB()
	if err != nil {
		log.Fatalf("初始化数据库失败: %v", err)
	}
	defer db.Close()

	config, err = loadConfig(ConfigPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	bot, err = tgbotapi.NewBotAPI(config.TelegramToken)
	if err != nil {
		log.Fatalf("初始化Telegram Bot失败: %v", err)
	}
	bot.Debug = false
	log.Printf("Telegram Bot已授权: %s", bot.Self.UserName)

	authorizedUsers[config.SuperAdminID] = true

	if err := loadSubscriptionsFromDB(); err != nil {
		log.Printf("加载订阅失败: %v", err)
	}
	if err := loadAuthorizedUsersFromDB(); err != nil {
		log.Printf("加载授权用户失败: %v", err)
	}

	go handleTelegramUpdates(config)

	for {
		time.Sleep(time.Duration(config.PollingInterval) * time.Second)
		monitorAllWallets()
	}
}

func initDB() (*sql.DB, error) {
	db, err := sql.Open("sqlite3", DBPath)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %v", err)
	}

	_, err = db.Exec(`
        CREATE TABLE IF NOT EXISTS subscriptions (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            chat_id TEXT NOT NULL,
            address TEXT NOT NULL,
            name TEXT NOT NULL,
            UNIQUE(chat_id, address)
        )
    `)
	if err != nil {
		return nil, fmt.Errorf("创建订阅表失败: %v", err)
	}

	_, err = db.Exec(`
        CREATE TABLE IF NOT EXISTS account_states (
            address TEXT PRIMARY KEY,
            account_value REAL,
            positions TEXT
        )
    `)
	if err != nil {
		return nil, fmt.Errorf("创建状态表失败: %v", err)
	}

	_, err = db.Exec(`
        CREATE TABLE IF NOT EXISTS authorized_users (
            chat_id TEXT PRIMARY KEY
        )
    `)
	if err != nil {
		return nil, fmt.Errorf("创建授权用户表失败: %v", err)
	}

	return db, nil
}

func loadConfig(path string) (*Config, error) {
	file, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %v", err)
	}

	var config Config
	if err := json.Unmarshal(file, &config); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %v", err)
	}

	if config.PollingInterval <= 0 {
		config.PollingInterval = 30
	}

	return &config, nil
}

func handleTelegramUpdates(config *Config) {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		if update.Message == nil {
			continue
		}

		chatID := strconv.FormatInt(update.Message.Chat.ID, 10)
		msgText := update.Message.Text

		switch {
		case msgText == "/myid":
			sendMessage(chatID, fmt.Sprintf("您的Chat ID是: %s", chatID))

		case strings.HasPrefix(msgText, "/authorize") && chatID == config.SuperAdminID:
			parts := strings.SplitN(msgText, " ", 2)
			if len(parts) < 2 {
				sendMessage(chatID, "用法: /authorize <chat_id>")
				continue
			}
			targetChatID := parts[1]
			authorizeUser(targetChatID)

		case strings.HasPrefix(msgText, "/deauthorize") && chatID == config.SuperAdminID:
			parts := strings.SplitN(msgText, " ", 2)
			if len(parts) < 2 {
				sendMessage(chatID, "用法: /deauthorize <chat_id>")
				continue
			}
			targetChatID := parts[1]
			deauthorizeUser(targetChatID)

		case strings.HasPrefix(msgText, "/subscribe"):
			if !isAuthorized(chatID) {
				sendMessage(chatID, "您没有权限订阅。请联系超级管理员 @imliyi 授权。")
				continue
			}
			parts := strings.SplitN(msgText, " ", 3)
			if len(parts) < 2 {
				sendMessage(chatID, "用法: /subscribe <地址> [名称]")
				continue
			}
			address := parts[1]
			if !isValidHexadecimal(address) {
				sendMessage(chatID, "无效的地址格式。")
				continue
			}
			name := "未命名账户"
			if len(parts) == 3 {
				name = parts[2]
			}
			subscribeWallet(chatID, address, name)

		case msgText == "/list":
			listSubscriptions(chatID)

		case strings.HasPrefix(msgText, "/unsubscribe"):
			parts := strings.SplitN(msgText, " ", 2)
			if len(parts) < 2 {
				sendMessage(chatID, "用法: /unsubscribe <地址>")
				continue
			}
			if !isValidHexadecimal(parts[1]) {
				sendMessage(chatID, "无效的地址格式。")
				continue
			}
			unsubscribeWallet(chatID, parts[1])

		case msgText == "/start" || msgText == "/help":
			message := "欢迎使用 Position Monitor 监控机器人!\n\n命令:\n/myid - 获取您的Chat ID\n/subscribe <地址> [名称] - 订阅一个地址（需要授权）\n/unsubscribe <地址> - 取消订阅\n/list - 查看已订阅地址\n\n超级管理员命令:\n/authorize <chat_id> - 授权用户\n/deauthorize <chat_id> - 取消授权"
			sendMessage(chatID, message)
		}
	}
}

func loadSubscriptionsFromDB() error {
	walletMutex.Lock()
	defer walletMutex.Unlock()

	rows, err := db.Query("SELECT chat_id, address, name FROM subscriptions")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var chatID, address, name string
		if err := rows.Scan(&chatID, &address, &name); err != nil {
			return err
		}
		key := chatID + "_" + address
		wallets[key] = WalletConfig{
			Address: address,
			Name:    name,
			ChatID:  chatID,
		}

		// 只加载一次状态
		if _, exists := accountStates[address]; !exists {
			var accountValue float64
			var positionsJSON string
			err := db.QueryRow("SELECT account_value, positions FROM account_states WHERE address = ?", address).
				Scan(&accountValue, &positionsJSON)
			if err != nil && err != sql.ErrNoRows {
				return err
			}

			positions := make(map[string]Position)
			if positionsJSON != "" {
				if err := json.Unmarshal([]byte(positionsJSON), &positions); err != nil {
					return err
				}
			}

			accountStates[address] = &AccountState{
				LastPositions:    positions,
				LastAccountValue: accountValue,
			}
		}
	}
	return nil
}

func loadAuthorizedUsersFromDB() error {
	walletMutex.Lock()
	defer walletMutex.Unlock()

	rows, err := db.Query("SELECT chat_id FROM authorized_users")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var chatID string
		if err := rows.Scan(&chatID); err != nil {
			return err
		}
		authorizedUsers[chatID] = true
	}
	return nil
}

func saveSubscriptionToDB(chatID, address, name string) error {
	_, err := db.Exec(`
        INSERT OR REPLACE INTO subscriptions (chat_id, address, name)
        VALUES (?, ?, ?)
    `, chatID, address, name)
	return err
}

func deleteSubscriptionFromDB(chatID, address string) error {
	_, err := db.Exec(`
        DELETE FROM subscriptions
        WHERE chat_id = ? AND address = ?
    `, chatID, address)
	return err
}

func saveAccountStateToDB(address string, state *AccountState) error {
	positionsJSON, err := json.Marshal(state.LastPositions)
	if err != nil {
		return err
	}

	_, err = db.Exec(`
        INSERT OR REPLACE INTO account_states (address, account_value, positions)
        VALUES (?, ?, ?)
    `, address, state.LastAccountValue, string(positionsJSON))
	return err
}

func authorizeUser(chatID string) {
	walletMutex.Lock()
	defer walletMutex.Unlock()

	authorizedUsers[chatID] = true
	_, err := db.Exec("INSERT OR IGNORE INTO authorized_users (chat_id) VALUES (?)", chatID)
	if err != nil {
		log.Printf("保存授权用户到数据库失败: %v", err)
	}
	sendMessage(chatID, "您已被超级管理员授权可以使用订阅功能！")
	sendMessage(config.SuperAdminID, fmt.Sprintf("已授权用户: %s", chatID))
}

func deauthorizeUser(chatID string) {
	walletMutex.Lock()
	defer walletMutex.Unlock()

	if chatID == config.SuperAdminID {
		sendMessage(chatID, "不能取消超级管理员的授权！")
		return
	}

	delete(authorizedUsers, chatID)
	_, err := db.Exec("DELETE FROM authorized_users WHERE chat_id = ?", chatID)
	if err != nil {
		log.Printf("从数据库删除授权用户失败: %v", err)
	}
	sendMessage(chatID, "您的授权已被超级管理员取消！")
	sendMessage(config.SuperAdminID, fmt.Sprintf("已取消用户授权: %s", chatID))
}

func isAuthorized(chatID string) bool {
	walletMutex.Lock()
	defer walletMutex.Unlock()
	return authorizedUsers[chatID]
}

func subscribeWallet(chatID, address, name string) {
	walletMutex.Lock()
	defer walletMutex.Unlock()

	key := chatID + "_" + address
	if _, exists := wallets[key]; exists {
		sendMessage(chatID, fmt.Sprintf("地址 %s 已订阅", shortenAddress(address)))
		return
	}

	wallet := WalletConfig{
		Address: address,
		Name:    name,
		ChatID:  chatID,
	}
	wallets[key] = wallet

	if err := saveSubscriptionToDB(chatID, address, name); err != nil {
		log.Printf("保存订阅到数据库失败: %v", err)
	}

	// 如果是第一个订阅该地址的用户，初始化状态
	if _, exists := accountStates[address]; !exists {
		accountStates[address] = &AccountState{
			LastPositions:    make(map[string]Position),
			LastAccountValue: 0,
		}
	}

	go func() {
		currentPositions, currentAccountValue, err := fetchPositions(address)
		if err != nil {
			log.Printf("首次获取 %s 持仓失败: %v", address, err)
			sendMessage(chatID, fmt.Sprintf("获取地址 %s 初始状态失败: %v", shortenAddress(address), err))
			return
		}

		// 发送初始状态给新订阅用户
		message := generateInitialStatusMessage(wallet, currentPositions, currentAccountValue)
		err = sendMessage(chatID, message)
		if err != nil {
			log.Printf("发送初始状态失败 %s: %v", address, err)
		}

		// 如果是第一个订阅者，更新状态
		if len(wallets) == 1 || !hasSubscribers(address, chatID) {
			accountStates[address].LastPositions = currentPositions
			accountStates[address].LastAccountValue = currentAccountValue
			if err := saveAccountStateToDB(address, accountStates[address]); err != nil {
				log.Printf("保存账户状态失败 %s: %v", address, err)
			}
		}
	}()

	sendMessage(chatID, fmt.Sprintf("已订阅地址 %s (%s)", shortenAddress(address), name))
}

// 检查是否有其他订阅者
func hasSubscribers(address, excludeChatID string) bool {
	for key := range wallets {
		wallet := wallets[key]
		if wallet.Address == address && wallet.ChatID != excludeChatID {
			return true
		}
	}
	return false
}

func unsubscribeWallet(chatID, address string) {
	walletMutex.Lock()
	defer walletMutex.Unlock()

	key := chatID + "_" + address
	if _, exists := wallets[key]; !exists {
		sendMessage(chatID, fmt.Sprintf("地址 %s 未被订阅", shortenAddress(address)))
		return
	}

	delete(wallets, key)
	if err := deleteSubscriptionFromDB(chatID, address); err != nil {
		log.Printf("从数据库删除订阅失败: %v", err)
	}

	// 如果没有其他订阅者，清理状态
	if !hasSubscribers(address, "") {
		delete(accountStates, address)
		_, err := db.Exec("DELETE FROM account_states WHERE address = ?", address)
		if err != nil {
			log.Printf("删除账户状态失败 %s: %v", address, err)
		}
	}

	sendMessage(chatID, fmt.Sprintf("已取消订阅地址 %s", shortenAddress(address)))
}

func listSubscriptions(chatID string) {
	walletMutex.Lock()
	defer walletMutex.Unlock()

	message := "📋 您的订阅列表:\n\n"
	count := 0
	for key, wallet := range wallets {
		if strings.HasPrefix(key, chatID+"_") {
			count++
			message += fmt.Sprintf("%d. %s - %s\n", count, wallet.Address, wallet.Name)
		}
	}
	if count == 0 {
		message = "您尚未订阅任何地址。"
	}
	sendMessage(chatID, message)
}

func monitorAllWallets() {
	walletMutex.Lock()
	walletsCopy := make(map[string]WalletConfig)
	for k, v := range wallets {
		walletsCopy[k] = v
	}
	walletMutex.Unlock()

	// 按地址聚合订阅者
	addressSubscribers := make(map[string][]WalletConfig)
	for _, wallet := range walletsCopy {
		addressSubscribers[wallet.Address] = append(addressSubscribers[wallet.Address], wallet)
	}

	// 对每个地址只获取一次数据
	for address, subscribers := range addressSubscribers {
		currentPositions, currentAccountValue, err := fetchPositions(address)
		if err != nil {
			log.Printf("监控 %s 失败: %v", address, err)
			continue
		}

		state, exists := accountStates[address]
		if !exists {
			// 如果状态不存在，可能是新地址，直接初始化并通知所有订阅者
			state = &AccountState{
				LastPositions:    make(map[string]Position),
				LastAccountValue: 0,
			}
			accountStates[address] = state
		}

		changes := detectPositionChanges(subscribers[0], currentPositions, currentAccountValue, state)
		if changes != "" {
			// 通知所有订阅该地址的用户
			for _, wallet := range subscribers {
				changes = detectPositionChanges(wallet, currentPositions, currentAccountValue, state)
				err = sendMessage(wallet.ChatID, changes)
				if err != nil {
					log.Printf("发送变化通知失败 %s (ChatID: %s): %v", address, wallet.ChatID, err)
				}
			}
			// 更新状态
			state.LastPositions = currentPositions
			state.LastAccountValue = currentAccountValue
			if err := saveAccountStateToDB(address, state); err != nil {
				log.Printf("保存账户状态失败 %s: %v", address, err)
			}
		}
	}
}

func sendMessage(chatID, message string) error {
	msg := tgbotapi.NewMessageToChannel(chatID, message)
	_, err := bot.Send(msg)
	return err
}

func generateInitialStatusMessage(wallet WalletConfig, positions map[string]Position, accountValue float64) string {
	timeStamp := time.Now().Format("2006-01-02 15:04:05")
	message := fmt.Sprintf("🔄 HyperLiquid初始持仓状态 - %s (%s)\n\n", wallet.Name, timeStamp)
	message += fmt.Sprintf("💼 账户地址: %s\n", shortenAddress(wallet.Address))
	message += fmt.Sprintf("💰 账户价值: $%.2f\n\n", accountValue)

	if len(positions) > 0 {
		message += "📊 当前持仓:\n\n"
		for _, position := range positions {
			szi, _ := strconv.ParseFloat(position.Szi, 64)
			entryPx, _ := strconv.ParseFloat(position.EntryPx, 64)
			posValue, _ := strconv.ParseFloat(position.PositionValue, 64)
			unrealizedPnl, _ := strconv.ParseFloat(position.UnrealizedPnl, 64)
			roi, _ := strconv.ParseFloat(position.ReturnOnEquity, 64)
			liquidationPx, _ := strconv.ParseFloat(position.LiquidationPx, 64)
			marginUsed, _ := strconv.ParseFloat(position.MarginUsed, 64)

			direction := "多头"
			if szi < 0 {
				direction = "空头"
				szi = -szi
			}

			message += fmt.Sprintf("🪙 %s (%s)\n", position.Coin, direction)
			message += fmt.Sprintf("📈 仓位大小: %.5f ($%.2f)\n", szi, posValue)
			message += fmt.Sprintf("🏷️ 入场价格: $%.2f\n", entryPx)
			message += fmt.Sprintf("📊 杠杆: %dx (%s)\n", position.Leverage.Value, position.Leverage.Type)
			pnlEmoji := "🔴"
			if unrealizedPnl >= 0 {
				pnlEmoji = "🟢"
			}
			message += fmt.Sprintf("%s 盈亏: $%.2f (%.2f%%)\n", pnlEmoji, unrealizedPnl, roi*100)
			message += fmt.Sprintf("⚠️ 强平价格: $%.2f\n", liquidationPx)
			message += fmt.Sprintf("💸 已用保证金: $%.2f\n\n", marginUsed)
		}
	} else {
		message += "没有找到开放的持仓。\n"
	}
	message += "🔔 持仓监控已启动，将在仓位变化时发送通知。"
	return message
}

func fetchPositions(address string) (map[string]Position, float64, error) {
	requestData := ClearinghouseRequest{
		Type: "clearinghouseState",
		User: address,
	}

	jsonData, err := json.Marshal(requestData)
	if err != nil {
		return nil, 0, fmt.Errorf("转换JSON时出错: %v", err)
	}

	req, err := http.NewRequest("POST", ApiEndpoint, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, 0, fmt.Errorf("创建请求时出错: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("发送请求时出错: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("读取响应时出错: %v", err)
	}

	var responseData Response
	if err := json.Unmarshal(body, &responseData); err != nil {
		return nil, 0, fmt.Errorf("解析响应时出错: %v", err)
	}

	accountValue, _ := strconv.ParseFloat(responseData.MarginSummary.AccountValue, 64)
	positions := make(map[string]Position)
	for _, pos := range responseData.AssetPositions {
		positions[pos.Position.Coin] = pos.Position
	}

	return positions, accountValue, nil
}

func detectPositionChanges(wallet WalletConfig, currentPositions map[string]Position, currentAccountValue float64, state *AccountState) string {
	changes := ""
	timeStamp := time.Now().Format("2006-01-02 15:04:05")

	for coin, current := range currentPositions {
		last, exists := state.LastPositions[coin]
		if !exists {
			if changes == "" {
				changes = fmt.Sprintf("🔄 HyperLiquid持仓变化 - %s (%s)\n\n", wallet.Name, timeStamp)
				changes += fmt.Sprintf("💼 账户地址: %s\n\n", shortenAddress(wallet.Address))
			}
			changes += fmt.Sprintf("🆕 新开仓位: %s\n", coin)
			addPositionDetails(&changes, current)
			continue
		}

		currentSzi, _ := strconv.ParseFloat(current.Szi, 64)
		lastSzi, _ := strconv.ParseFloat(last.Szi, 64)
		sziChangePercent := 0.0
		if lastSzi != 0 {
			sziChangePercent = math.Abs((currentSzi-lastSzi)/lastSzi) * 100
		}

		if sziChangePercent >= 1.0 {
			if changes == "" {
				changes = fmt.Sprintf("🔄 HyperLiquid持仓变化 - %s (%s)\n\n", wallet.Name, timeStamp)
				changes += fmt.Sprintf("💼 账户地址: %s\n\n", shortenAddress(wallet.Address))
			}

			if math.Abs(currentSzi) > math.Abs(lastSzi) {
				changes += fmt.Sprintf("📈 仓位增加: %s\n", coin)
			} else {
				changes += fmt.Sprintf("📉 仓位减少: %s\n", coin)
			}
			changes += fmt.Sprintf("   从: %.5f\n", lastSzi)
			changes += fmt.Sprintf("   到: %.5f\n", currentSzi)
			changes += fmt.Sprintf("   变化: %.2f%%\n\n", sziChangePercent)
		}
	}

	for coin := range state.LastPositions {
		if _, exists := currentPositions[coin]; !exists {
			if changes == "" {
				changes = fmt.Sprintf("🔄 HyperLiquid持仓变化 - %s (%s)\n\n", wallet.Name, timeStamp)
				changes += fmt.Sprintf("💼 账户地址: %s\n\n", shortenAddress(wallet.Address))
			}
			changes += fmt.Sprintf("❌ 已关闭仓位: %s\n\n", coin)
		}
	}

	return changes
}

func addPositionDetails(message *string, position Position) {
	szi, _ := strconv.ParseFloat(position.Szi, 64)
	entryPx, _ := strconv.ParseFloat(position.EntryPx, 64)
	posValue, _ := strconv.ParseFloat(position.PositionValue, 64)
	unrealizedPnl, _ := strconv.ParseFloat(position.UnrealizedPnl, 64)
	roi, _ := strconv.ParseFloat(position.ReturnOnEquity, 64)
	liquidationPx, _ := strconv.ParseFloat(position.LiquidationPx, 64)

	direction := "多头"
	if szi < 0 {
		direction = "空头"
		szi = -szi
	}

	*message += fmt.Sprintf("   %s (%s)\n", position.Coin, direction)
	*message += fmt.Sprintf("   📈 仓位大小: %.5f ($%.2f)\n", szi, posValue)
	*message += fmt.Sprintf("   🏷️ 入场价格: $%.2f\n", entryPx)
	*message += fmt.Sprintf("   📊 杠杆: %dx\n", position.Leverage.Value)
	pnlEmoji := "🔴"
	if unrealizedPnl >= 0 {
		pnlEmoji = "🟢"
	}
	*message += fmt.Sprintf("   %s 盈亏: $%.2f (%.2f%%)\n", pnlEmoji, unrealizedPnl, roi*100)
	*message += fmt.Sprintf("   ⚠️ 强平价格: $%.2f\n\n", liquidationPx)
}

func shortenAddress(address string) string {
	if len(address) <= 10 {
		return address
	}
	return address[:6] + "..." + address[len(address)-4:]
}

func isValidHexadecimal(input string) bool {
	re := regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
	return re.MatchString(input)
}
