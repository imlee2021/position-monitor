package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

// ClearinghouseRequest 请求和响应结构
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

// Config 结构，用于从配置文件中加载设置
type Config struct {
	TelegramToken   string         `json:"telegramToken"`
	ChatID          string         `json:"chatID"`
	PollingInterval int            `json:"pollingInterval"`
	Addresses       []WalletConfig `json:"addresses"`
}

// WalletConfig 结构，用于存储每个钱包地址的配置
type WalletConfig struct {
	Address string `json:"address"`
	Name    string `json:"name"`
}

// AccountState 存储每个账户的状态
type AccountState struct {
	LastPositions    map[string]Position
	LastAccountValue float64
}

// 配置常量
const (
	ApiEndpoint    = "https://api.hyperliquid.xyz/info"
	TelegramApiUrl = "https://api.telegram.org/bot%s/sendMessage"
	ConfigPath     = "config.json" // 配置文件路径
)

var accountStates map[string]*AccountState

func main() {
	// 加载配置
	config, err := loadConfig(ConfigPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	// 初始化账户状态
	accountStates = make(map[string]*AccountState)
	for _, wallet := range config.Addresses {
		accountStates[wallet.Address] = &AccountState{
			LastPositions:    make(map[string]Position),
			LastAccountValue: 0,
		}
	}

	log.Printf("开始HyperLiquid多账户持仓监控，间隔: %d秒，监控账户数: %d", config.PollingInterval, len(config.Addresses))

	// 对每个地址获取初始状态并发送
	for _, wallet := range config.Addresses {
		initialStatus(wallet, config.TelegramToken, config.ChatID)
	}

	// 持续监控
	for {
		time.Sleep(time.Duration(config.PollingInterval) * time.Second)

		for _, wallet := range config.Addresses {
			monitorAddress(wallet, config.TelegramToken, config.ChatID)
		}
	}
}

// 加载配置
func loadConfig(path string) (*Config, error) {
	file, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %v", err)
	}

	var config Config
	if err := json.Unmarshal(file, &config); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %v", err)
	}

	// 设置默认值
	if config.PollingInterval <= 0 {
		config.PollingInterval = 30 // 默认30秒
	}

	return &config, nil
}

// 初始化状态
func initialStatus(wallet WalletConfig, telegramToken, chatID string) {
	currentPositions, currentAccountValue, err := fetchPositions(wallet.Address)
	if err != nil {
		log.Printf("账户 %s (%s) 首次获取持仓信息失败: %v", wallet.Name, wallet.Address, err)
		return
	}

	// 生成并发送初始状态消息
	initialMessage := generateInitialStatusMessage(wallet, currentPositions, currentAccountValue)
	err = sendTelegramMessage(telegramToken, chatID, initialMessage)
	if err != nil {
		log.Printf("账户 %s (%s) 发送初始状态消息失败: %v", wallet.Name, wallet.Address, err)
	} else {
		log.Printf("账户 %s (%s) 初始持仓状态已发送", wallet.Name, wallet.Address)
	}

	// 保存为基准数据
	state := accountStates[wallet.Address]
	state.LastPositions = currentPositions
	state.LastAccountValue = currentAccountValue
}

// 监控单个地址
func monitorAddress(wallet WalletConfig, telegramToken, chatID string) {
	currentPositions, currentAccountValue, err := fetchPositions(wallet.Address)
	if err != nil {
		log.Printf("账户 %s (%s) 获取持仓信息失败: %v", wallet.Name, wallet.Address, err)
		return
	}

	// 获取账户状态
	state := accountStates[wallet.Address]

	// 检测变化
	changes := detectPositionChanges(wallet, currentPositions, currentAccountValue, state)
	if changes != "" {
		log.Printf("账户 %s (%s) 检测到持仓变化，发送通知", wallet.Name, wallet.Address)
		err = sendTelegramMessage(telegramToken, chatID, changes)
		if err != nil {
			log.Printf("账户 %s (%s) 发送Telegram消息时出错: %v", wallet.Name, wallet.Address, err)
		} else {
			log.Printf("账户 %s (%s) 持仓变化通知已发送", wallet.Name, wallet.Address)
			// 更新最后的持仓信息
			state.LastPositions = currentPositions
			state.LastAccountValue = currentAccountValue
		}
	}
}

// 生成初始状态消息
func generateInitialStatusMessage(wallet WalletConfig, positions map[string]Position, accountValue float64) string {
	timeStamp := time.Now().Format("2006-01-02 15:04:05")
	message := fmt.Sprintf("🔄 HyperLiquid初始持仓状态 - %s (%s)\n\n", wallet.Name, timeStamp)

	// 账户摘要
	withdrawable := 0.0
	// 如果响应中有可提取金额，可以在此解析

	message += fmt.Sprintf("💼 账户地址: %s\n", shortenAddress(wallet.Address))
	message += fmt.Sprintf("💰 账户价值: $%.2f\n", accountValue)
	message += fmt.Sprintf("💵 可提取金额: $%.2f\n\n", withdrawable)

	// 持仓详情
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

			// 确定持仓方向
			direction := "多头"
			if szi < 0 {
				direction = "空头"
				szi = -szi // 变为正数以便显示
			}

			message += fmt.Sprintf("🪙 %s (%s)\n", position.Coin, direction)
			message += fmt.Sprintf("📈 仓位大小: %.5f ($%.2f)\n", szi, posValue)
			message += fmt.Sprintf("🏷️ 入场价格: $%.2f\n", entryPx)
			message += fmt.Sprintf("📊 杠杆: %dx (%s)\n", position.Leverage.Value, position.Leverage.Type)

			// 根据盈亏正负选择表情
			pnlEmoji := "🔴"
			if unrealizedPnl >= 0 {
				pnlEmoji = "🟢"
			}
			message += fmt.Sprintf("%s 盈亏: $%.2f (%.2f%%)\n", pnlEmoji, unrealizedPnl, roi*100)

			message += fmt.Sprintf("⚠️ 强平价格: $%.2f\n", liquidationPx)
			message += fmt.Sprintf("💸 已用保证金: $%.2f\n", marginUsed)

			// 添加资金费率信息
			fundingAllTime, _ := strconv.ParseFloat(position.CumFunding.AllTime, 64)
			fundingSinceOpen, _ := strconv.ParseFloat(position.CumFunding.SinceOpen, 64)

			message += fmt.Sprintf("💰 资金费用: $%.2f (开仓后: $%.2f)\n\n", fundingAllTime, fundingSinceOpen)
		}
	} else {
		message += "没有找到开放的持仓。"
	}

	message += "\n🔔 持仓监控已启动，将在仓位变化时发送通知。"

	return message
}

// 获取当前持仓信息
func fetchPositions(address string) (map[string]Position, float64, error) {
	// 准备请求数据
	requestData := ClearinghouseRequest{
		Type: "clearinghouseState",
		User: address,
	}

	// 转换请求为JSON
	jsonData, err := json.Marshal(requestData)
	if err != nil {
		return nil, 0, fmt.Errorf("转换JSON时出错: %v", err)
	}

	// 创建HTTP请求
	req, err := http.NewRequest("POST", ApiEndpoint, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, 0, fmt.Errorf("创建请求时出错: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")

	// 发送请求
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("发送请求时出错: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 读取响应内容
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("读取响应时出错: %v", err)
	}

	// 解析响应
	var responseData Response
	if err := json.Unmarshal(body, &responseData); err != nil {
		return nil, 0, fmt.Errorf("解析响应时出错: %v", err)
	}

	// 提取账户价值
	accountValue, _ := strconv.ParseFloat(responseData.MarginSummary.AccountValue, 64)

	// 提取持仓信息并按币种索引
	positions := make(map[string]Position)
	for _, pos := range responseData.AssetPositions {
		positions[pos.Position.Coin] = pos.Position
	}

	return positions, accountValue, nil
}

// 检测持仓变化
func detectPositionChanges(wallet WalletConfig, currentPositions map[string]Position, currentAccountValue float64, state *AccountState) string {
	changes := ""
	timeStamp := time.Now().Format("2006-01-02 15:04:05")

	// 检查账户价值变化
	accountValueChange := currentAccountValue - state.LastAccountValue
	accountValueChangePercent := 0.0
	if state.LastAccountValue > 0 {
		accountValueChangePercent = (accountValueChange / state.LastAccountValue) * 100
	}

	// 如果账户价值变化超过1%，报告变化
	significantAccountChange := math.Abs(accountValueChangePercent) >= 1.0

	// 检查新增或修改的仓位
	newPositions := false
	modifiedPositions := false

	for coin, current := range currentPositions {
		// 检查是否是新仓位
		last, exists := state.LastPositions[coin]
		if !exists {
			if changes == "" {
				changes = fmt.Sprintf("🔄 HyperLiquid持仓变化 - %s (%s)\n\n", wallet.Name, timeStamp)
				changes += fmt.Sprintf("💼 账户地址: %s\n\n", shortenAddress(wallet.Address))
			}
			changes += fmt.Sprintf("🆕 新开仓位: %s\n", coin)
			addPositionDetails(&changes, current)
			newPositions = true
			continue
		}

		// 检查仓位大小变化
		currentSzi, _ := strconv.ParseFloat(current.Szi, 64)
		lastSzi, _ := strconv.ParseFloat(last.Szi, 64)

		// 如果仓位大小变化超过1%
		sziChangePercent := 0.0
		if lastSzi != 0 {
			sziChangePercent = math.Abs((currentSzi-lastSzi)/lastSzi) * 100
		}

		if sziChangePercent >= 1.0 {
			if changes == "" {
				changes = fmt.Sprintf("🔄 HyperLiquid持仓变化 - %s (%s)\n\n", wallet.Name, timeStamp)
				changes += fmt.Sprintf("💼 账户地址: %s\n\n", shortenAddress(wallet.Address))
			}

			// 仓位增加或减少
			if math.Abs(currentSzi) > math.Abs(lastSzi) {
				changes += fmt.Sprintf("📈 仓位增加: %s\n", coin)
			} else {
				changes += fmt.Sprintf("📉 仓位减少: %s\n", coin)
			}

			changes += fmt.Sprintf("   从: %.5f\n", lastSzi)
			changes += fmt.Sprintf("   到: %.5f\n", currentSzi)
			changes += fmt.Sprintf("   变化: %.2f%%\n\n", sziChangePercent)
			modifiedPositions = true
		}
	}

	// 检查关闭的仓位
	removedPositions := false
	for coin, last := range state.LastPositions {
		if _, exists := currentPositions[coin]; !exists {
			if changes == "" {
				changes = fmt.Sprintf("🔄 HyperLiquid持仓变化 - %s (%s)\n\n", wallet.Name, timeStamp)
				changes += fmt.Sprintf("💼 账户地址: %s\n\n", shortenAddress(wallet.Address))
			}
			changes += fmt.Sprintf("❌ 已关闭仓位: %s\n", coin)
			lastSzi, _ := strconv.ParseFloat(last.Szi, 64)
			changes += fmt.Sprintf("   仓位大小: %.5f\n\n", lastSzi)
			removedPositions = true
		}
	}

	// 如果有显著账户价值变化但没有持仓变化，单独报告
	if significantAccountChange && !newPositions && !modifiedPositions && !removedPositions {
		changes = fmt.Sprintf("🔄 HyperLiquid账户价值变化 - %s (%s)\n\n", wallet.Name, timeStamp)
		changes += fmt.Sprintf("💼 账户地址: %s\n\n", shortenAddress(wallet.Address))

		valueChangeEmoji := "🔴"
		if accountValueChange >= 0 {
			valueChangeEmoji = "🟢"
		}

		changes += fmt.Sprintf("%s 账户价值变化:\n", valueChangeEmoji)
		changes += fmt.Sprintf("   从: $%.2f\n", state.LastAccountValue)
		changes += fmt.Sprintf("   到: $%.2f\n", currentAccountValue)
		changes += fmt.Sprintf("   变化: $%.2f (%.2f%%)\n\n", accountValueChange, accountValueChangePercent)

		// 添加当前持仓摘要
		if len(currentPositions) > 0 {
			changes += "📊 当前持仓摘要:\n\n"
			for coin, position := range currentPositions {
				szi, _ := strconv.ParseFloat(position.Szi, 64)
				pnl, _ := strconv.ParseFloat(position.UnrealizedPnl, 64)
				roi, _ := strconv.ParseFloat(position.ReturnOnEquity, 64)

				direction := "多头"
				if szi < 0 {
					direction = "空头"
					szi = -szi
				}

				pnlEmoji := "🔴"
				if pnl >= 0 {
					pnlEmoji = "🟢"
				}

				changes += fmt.Sprintf("🪙 %s (%s): %.5f\n", coin, direction, szi)
				changes += fmt.Sprintf("   %s 盈亏: $%.2f (%.2f%%)\n\n", pnlEmoji, pnl, roi*100)
			}
		}
	}

	return changes
}

// 添加仓位详细信息到消息中
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
		szi = -szi // 变为正数以便显示
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

// 发送Telegram消息
func sendTelegramMessage(token, chatID, message string) error {
	telegramUrl := fmt.Sprintf(TelegramApiUrl, token)

	// 准备表单数据
	formData := url.Values{
		"chat_id": {chatID},
		"text":    {message},
	}

	// 发送POST请求到Telegram API
	resp, err := http.PostForm(telegramUrl, formData)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	// 检查响应状态
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram API错误: %s", string(body))
	}

	return nil
}

// 缩短地址显示
func shortenAddress(address string) string {
	if len(address) <= 10 {
		return address
	}
	return address[:6] + "..." + address[len(address)-4:]
}
