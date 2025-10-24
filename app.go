package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/unidoc/unioffice/v2/color"
	"github.com/unidoc/unioffice/v2/common/license"
	"github.com/unidoc/unioffice/v2/document"
	"github.com/unidoc/unioffice/v2/measurement"
	"github.com/unidoc/unioffice/v2/schema/soo/wml"
)

type Config struct {
	UseGigaChat bool           `json:"useGigaChat"`
	GigaChat    GigaChatConfig `json:"gigaChat"`
	Ollama      OllamaConfig   `json:"ollama"`
}
type GigaChatConfig struct {
	APIKey string `json:"apiKey"`
	Model  string `json:"model"`
}
type OllamaConfig struct {
	BaseURL string `json:"baseURL"`
	Model   string `json:"model"`
}

type GigaChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type GigaChatRequest struct {
	Model       string            `json:"model"`
	Messages    []GigaChatMessage `json:"messages"`
	Temperature float32           `json:"temperature"`
}
type GigaChatResponseChoice struct {
	Message GigaChatMessage `json:"message"`
}
type GigaChatResponse struct {
	Choices []GigaChatResponseChoice `json:"choices"`
}
type OllamaRequest struct {
	Model    string            `json:"model"`
	Messages []GigaChatMessage `json:"messages"`
	Stream   bool              `json:"stream"`
}
type OllamaResponse struct {
	Message GigaChatMessage `json:"message"`
}

type ParsedProduct struct {
	Name  string `json:"name"`
	Price int    `json:"price"`
}
type LLMResponseItem struct {
	ID       int `json:"id"`
	Quantity int `json:"quantity"`
}
type LLMResponse struct {
	FoundItems []LLMResponseItem `json:"found_items"`
}
type TCPItem struct {
	Name     string `json:"name"`
	Quantity int    `json:"quantity"`
	Price    int    `json:"price"`
	Subtotal int    `json:"subtotal"`
}
type Product struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Price int    `json:"price"`
}
type FinalLogResponse struct {
	FoundItems []TCPItem `json:"found_items"`
	TotalCost  int       `json:"total_cost"`
}
type LogEntry struct {
	Query    string           `json:"query"`
	Response FinalLogResponse `json:"response"`
}

type App struct {
	ctx            context.Context
	config         Config
	products       []Product
	productMap     map[int]Product
	httpClient     *resty.Client
	gigaToken      string
	tokenExpiresAt time.Time
	tokenMutex     sync.Mutex
	jsonExtractor  *regexp.Regexp
	dataLoadMutex  sync.Mutex
}

func createDefaultConfig() (Config, error) {
	log.Println("Файл 'config.json' не найден. Создаю файл с настройками по умолчанию...")
	defaultConfig := Config{
		UseGigaChat: true,
		GigaChat: GigaChatConfig{
			APIKey: "PASTE_YOUR_BASE64_GIGACHAT_API_KEY_HERE",
			Model:  "GigaChat:latest",
		},
		Ollama: OllamaConfig{
			BaseURL: "http://localhost:11434",
			Model:   "llama3",
		},
	}
	configData, err := json.MarshalIndent(defaultConfig, "", "  ")
	if err != nil {
		return Config{}, fmt.Errorf("не удалось создать JSON для конфига по умолчанию: %w", err)
	}
	err = os.WriteFile("config.json", configData, 0644)
	if err != nil {
		return Config{}, fmt.Errorf("не удалось записать config.json на диск: %w", err)
	}
	log.Println("Файл 'config.json' успешно создан. Пожалуйста, откройте его и вставьте ваш API ключ для GigaChat.")
	return defaultConfig, nil
}

func loadConfig() (Config, error) {
	var config Config
	configFile, err := os.ReadFile("config.json")
	if err != nil {
		if os.IsNotExist(err) {
			return createDefaultConfig()
		}
		return config, fmt.Errorf("не удалось прочитать config.json: %w", err)
	}
	err = json.Unmarshal(configFile, &config)
	if err != nil {
		return config, fmt.Errorf("не удалось распарсить config.json: %w", err)
	}
	log.Println("Конфигурация успешно загружена из 'config.json'.")
	return config, nil
}

func NewApp() *App {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("КРИТИЧЕСКАЯ ОШИБКА: не удалось загрузить конфигурацию: %v", err)
	}
	client := resty.New()
	client.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: true})
	jsonRe := regexp.MustCompile(`(?s)({.*}|\[.*\])`)
	return &App{
		config:        cfg,
		httpClient:    client,
		jsonExtractor: jsonRe,
	}
}

func init() {
	unidocKey := "80052ea1203bf3421824c05723e5258fbacf3d126e6b5a8e49517dafcc81fee5"
	err := license.SetMeteredKey(unidocKey)
	if err != nil {
		log.Fatalf("КРИТИЧЕСКАЯ ОШИБКА: не удалось активировать лицензию UniOffice: %v", err)
	}
	log.Println("Лицензия UniOffice успешно активирована.")
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.loadProductsFromCache()
}

func (a *App) GenerateAndCreateFiles(clientRequest string) (string, error) {
	err := a.ensureDataIsLoaded()
	if err != nil {
		return "", err
	}
	relevantProducts := a.retrieveRelevantProducts(clientRequest, 50)
	if len(relevantProducts) == 0 {
		return "", fmt.Errorf("не удалось найти ни одного релевантного товара для запроса: '%s'. Попробуйте переформулировать запрос", clientRequest)
	}
	productsJSON, _ := json.Marshal(relevantProducts)

	planningPrompt := `Ты — главный инженер по комплектации заказов. Твоя репутация зависит от того, насколько полно и правильно ты соберешь заказ для клиента.

**ТВОЯ ГЛАВНАЯ ЗАДАЧА:**
Проанализируй **цель клиента** и, используя предоставленный тебе **список РЕЛЕВАНТНЫХ товаров со склада**, составь **исчерпывающий список всего, что ему потребуется**.

-   Если цель — **конкретная деталь** ("Крышка 200 мм"), твой список должен состоять **только из этой детали**.
-   Если цель — **монтаж или сборка** ("комплект для монтажа короба 200х200"), твоя обязанность — включить в список **ВСЕ** необходимые для этого компоненты из предложенного каталога: сам короб, крышку, винты и гайки. Ты несешь ответственность за полноту комплекта.

**ПРАВИЛА ОФОРМЛЕНИЯ:**
-   Твой ответ — **ТОЛЬКО маркированный список** в формате '- Название, Количество'.
-   **ЗАПРЕЩЕНО:** Никаких заголовков, комментариев или пустых строк.

---
**Список релевантных товаров (выборка со склада):**
%s

**Запрос (цель клиента):**
"%s"
---
**Твой итоговый список комплектации:**
`
	fullPlanningPrompt := fmt.Sprintf(planningPrompt, string(productsJSON), clientRequest)

	log.Println("Этап 1 (RAG): Запрос плана комплектации...")
	engineeringPlan, err := a.callLLMForText(fullPlanningPrompt)
	if err != nil {
		return "", fmt.Errorf("ошибка на этапе 1 (планирование): %w", err)
	}
	log.Printf("Получен план:\n---\n%s\n---", engineeringPlan)

	finalJsonPrompt := `Ты — ассистент по обработке данных. Твоя задача — на основе **плана комплектации** и **JSON-списка РЕЛЕВАНТНЫХ товаров** сгенерировать итоговый JSON.

**ПРАВИЛА:**
1.  **СТРОГО СЛЕДУЙ ПЛАНУ.** Включай в ответ только те позиции, которые упомянуты в плане.
2.  **ТОЧНОЕ СОПОСТАВЛЕНИЕ.** Найди в JSON-списке товары, которые максимально точно соответствуют описанию в плане.
3.  **ТОЛЬКО JSON.** Твой ответ должен быть только валидным JSON-объектом без лишних символов и комментариев.

**Формат ответа:**
` + "```json" + `
{
  "found_items": [
    {"id": 15, "quantity": 10}
  ]
}
` + "```" + `
---
**План для обработки:**
%s

**JSON-список релевантных товаров:**
%s
---
`
	fullFinalJsonPrompt := fmt.Sprintf(finalJsonPrompt, engineeringPlan, string(productsJSON))

	log.Println("Этап 2 (RAG): Запрос финального JSON...")
	llmResponseJSON, err := a.callLLMForJSON(fullFinalJsonPrompt)
	if err != nil {
		return "", fmt.Errorf("ошибка на этапе 2 (форматирование JSON): %w", err)
	}

	var llmResponse LLMResponse
	if err := json.Unmarshal([]byte(llmResponseJSON), &llmResponse); err != nil {
		return "", fmt.Errorf("LLM вернула невалидный JSON: %w. Ответ: %s", err, llmResponseJSON)
	}

	var finalItems []TCPItem
	var totalCost int
	for _, item := range llmResponse.FoundItems {
		product, exists := a.productMap[item.ID]
		if !exists {
			log.Printf("ПРЕДУПРЕЖДЕНИЕ: LLM вернула несуществующий ID: %d. Позиция пропущена.", item.ID)
			continue
		}
		subtotal := product.Price * item.Quantity
		finalItems = append(finalItems, TCPItem{
			Name:     product.Name,
			Quantity: item.Quantity,
			Price:    product.Price,
			Subtotal: subtotal,
		})
		totalCost += subtotal
	}

	err = a.saveLogFile(clientRequest, finalItems, totalCost)
	if err != nil {
		log.Printf("ПРЕДУПРЕЖДЕНИЕ: не удалось сохранить лог-файл: %v", err)
	}

	log.Println("Генерация DOCX файла с таблицей...")
	base64Docx, err := a.createStyledDocxFile(finalItems, totalCost)
	if err != nil {
		return "", fmt.Errorf("ошибка создания DOCX файла: %w", err)
	}

	log.Println("DOCX файл успешно создан и отправлен на фронтенд.")
	return base64Docx, nil
}

func (a *App) callLLMForJSON(prompt string) (string, error) {
	rawContent, err := a.callLLMForText(prompt)
	if err != nil {
		return "", err
	}
	jsonMatch := a.jsonExtractor.FindString(rawContent)
	if jsonMatch == "" {
		return "", fmt.Errorf("не удалось найти JSON в ответе LLM. Ответ был: %s", rawContent)
	}
	return jsonMatch, nil
}

func (a *App) callLLMForText(prompt string) (string, error) {
	if a.config.UseGigaChat {
		log.Println("Используется API: GigaChat")
		return a.callGigaChatAPIInternal(prompt)
	}
	log.Println("Используется API: Ollama")
	return a.callOllamaAPIInternal(prompt)
}

func (a *App) callGigaChatAPIInternal(prompt string) (string, error) {
	token, err := a.getAccessToken()
	if err != nil {
		return "", err
	}
	requestBody := GigaChatRequest{
		Model:       a.config.GigaChat.Model,
		Messages:    []GigaChatMessage{{Role: "user", Content: prompt}},
		Temperature: 0.1,
	}
	resp, err := a.httpClient.R().
		SetHeader("Content-Type", "application/json").
		SetHeader("Accept", "application/json").
		SetAuthToken(token).
		SetBody(requestBody).
		Post("https://gigachat.devices.sberbank.ru/api/v1/chat/completions")

	if err != nil {
		return "", fmt.Errorf("сетевая ошибка при вызове GigaChat: %w", err)
	}
	log.Printf("RAW RESPONSE BODY FROM GIGACHAT:\n%s\n", resp.String())
	if resp.IsError() {
		return "", fmt.Errorf("ошибка от API GigaChat: %s - %s", resp.Status(), resp.String())
	}
	var gigaResponse GigaChatResponse
	if err := json.Unmarshal(resp.Body(), &gigaResponse); err != nil {
		return "", fmt.Errorf("ошибка парсинга ответа GigaChat: %w", err)
	}
	if len(gigaResponse.Choices) == 0 {
		return "", fmt.Errorf("GigaChat вернул пустой ответ")
	}
	return gigaResponse.Choices[0].Message.Content, nil
}

func (a *App) callOllamaAPIInternal(prompt string) (string, error) {
	requestBody := OllamaRequest{
		Model:    a.config.Ollama.Model,
		Messages: []GigaChatMessage{{Role: "user", Content: prompt}},
		Stream:   false,
	}
	apiURL := a.config.Ollama.BaseURL + "/api/chat"
	resp, err := a.httpClient.R().
		SetHeader("Content-Type", "application/json").
		SetBody(requestBody).
		Post(apiURL)

	if err != nil {
		return "", fmt.Errorf("сетевая ошибка при вызове Ollama: %w", err)
	}
	log.Printf("RAW RESPONSE BODY FROM OLLAMA:\n%s\n", resp.String())
	if resp.IsError() {
		return "", fmt.Errorf("ошибка от API Ollama: %s - %s", resp.Status(), resp.String())
	}
	var ollamaResponse OllamaResponse
	if err := json.Unmarshal(resp.Body(), &ollamaResponse); err != nil {
		return "", fmt.Errorf("ошибка парсинга ответа Ollama: %w", err)
	}
	return ollamaResponse.Message.Content, nil
}

func (a *App) retrieveRelevantProducts(query string, topK int) []Product {
	keywords, err := a.extractKeywordsWithLLM(query)
	if err != nil {
		log.Printf("ПРЕДУПРЕЖДЕНИЕ: Не удалось извлечь ключевые слова через LLM, переключаюсь на простой поиск. Ошибка: %v", err)
		keywords = tokenize(query)
	}

	if len(keywords) == 0 {
		return []Product{}
	}

	type ScoredProduct struct {
		Product Product
		Score   int
	}
	var scoredProducts []ScoredProduct

	for _, product := range a.products {
		score := 0
		productNameLower := strings.ToLower(product.Name)

		for _, keyword := range keywords {
			if strings.Contains(productNameLower, keyword) {
				score++
			}
		}

		if score > 0 {
			scoredProducts = append(scoredProducts, ScoredProduct{Product: product, Score: score})
		}
	}

	sort.Slice(scoredProducts, func(i, j int) bool {
		if scoredProducts[i].Score != scoredProducts[j].Score {
			return scoredProducts[i].Score > scoredProducts[j].Score
		}
		return len(scoredProducts[i].Product.Name) < len(scoredProducts[j].Product.Name)
	})

	var relevantProducts []Product
	limit := topK
	if len(scoredProducts) < topK {
		limit = len(scoredProducts)
	}
	for i := 0; i < limit; i++ {
		relevantProducts = append(relevantProducts, scoredProducts[i].Product)
	}

	log.Printf("RAG Этап 2: Найдено %d товаров по ключевым словам от LLM. Передаю для финальной сборки.", len(relevantProducts))
	return relevantProducts
}

type LLMKeywordResponse struct {
	Keywords []string `json:"keywords"`
}

func (a *App) extractKeywordsWithLLM(query string) ([]string, error) {
	prompt := `Твоя задача — проанализировать запрос клиента для поиска товаров на складе и извлечь из него только самые важные, уникальные ключевые слова.

ПРАВИЛА:
1.  **ИЗВЛЕКАЙ СУЩНОСТЬ:** Выделяй только существительные, прилагательные и технические обозначения (артикулы, размеры).
2.  **ИГНОРИРУЙ МУСОР:** Полностью игнорируй количество ("10 штук", "12 метров"), единицы измерения ("мм"), предлоги, союзы ("и", "для") и любые разговорные фразы ("мне нужно", "пожалуйста").
3.  **ИСПРАВЛЯЙ ОПЕЧАТКИ:** Если видишь явную опечатку (например, "крыжка"), исправь ее ("крышка").
4.  **ФОРМАТ ОТВЕТА:** Верни ТОЛЬКО валидный JSON-объект с одним полем "keywords", которое содержит массив извлеченных слов в нижнем регистре. Никакого текста до или после JSON.

ПРИМЕР:
Запрос клиента: "Лоток перфорированый 100х100, 12 метров, и 10 гаек М10"
Твой ответ:
` + "```json" + `
{
  "keywords": ["лоток", "перфорированный", "100х100", "гайка", "м10"]
}
` + "```" + `

---
ЗАПРОС КЛИЕНТА ДЛЯ ОБРАБОТКИ:
"%s"
---
ТВОЙ JSON-ОТВЕТ:
`
	fullPrompt := fmt.Sprintf(prompt, query)

	log.Println("RAG Этап 1: Извлечение ключевых слов через LLM...")

	jsonResponse, err := a.callLLMForJSON(fullPrompt)
	if err != nil {
		return nil, fmt.Errorf("LLM не смогла извлечь ключевые слова: %w", err)
	}

	var keywordResponse LLMKeywordResponse
	if err := json.Unmarshal([]byte(jsonResponse), &keywordResponse); err != nil {
		return nil, fmt.Errorf("не удалось распарсить JSON с ключевыми словами: %w. Ответ был: %s", err, jsonResponse)
	}

	log.Printf("LLM извлекла ключевые слова: %v", keywordResponse.Keywords)
	return keywordResponse.Keywords, nil
}

func tokenize(text string) []string {
	lowerText := strings.ToLower(text)
	r := strings.NewReplacer(",", " ", ".", " ", "(", " ", ")", " ", "/", " ", "\\", " ", "[", " ", "]", " ", "-", " ", "\"", " ")
	textWithSpaces := r.Replace(lowerText)
	return strings.Fields(textWithSpaces)
}

func (a *App) getAccessToken() (string, error) {
	a.tokenMutex.Lock()
	defer a.tokenMutex.Unlock()
	if a.gigaToken != "" && time.Now().Before(a.tokenExpiresAt) {
		return a.gigaToken, nil
	}
	log.Println("Токен GigaChat истек или отсутствует. Получение нового токена...")
	resp, err := a.httpClient.R().
		SetHeader("Content-Type", "application/x-www-form-urlencoded").
		SetHeader("Accept", "application/json").
		SetHeader("RqUID", "a1a2a3a4-a5a6-a7a8-a9a0-a1a2a3a4a5a6").
		SetHeader("Authorization", "Basic "+a.config.GigaChat.APIKey).
		SetFormData(map[string]string{
			"scope": "GIGACHAT_API_PERS",
		}).
		Post("https://ngw.devices.sberbank.ru:9443/api/v2/oauth")
	if err != nil {
		return "", fmt.Errorf("ошибка при запросе токена: %w", err)
	}
	if resp.IsError() {
		return "", fmt.Errorf("ошибка от API при получении токена: %s - %s", resp.Status(), resp.String())
	}
	var tokenResponse struct {
		AccessToken string `json:"access_token"`
		ExpiresAt   int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(resp.Body(), &tokenResponse); err != nil {
		return "", fmt.Errorf("ошибка парсинга ответа с токеном: %w", err)
	}
	a.gigaToken = tokenResponse.AccessToken
	a.tokenExpiresAt = time.Unix(tokenResponse.ExpiresAt/1000, 0).Add(-1 * time.Minute)
	log.Println("Новый токен GigaChat успешно получен.")
	return a.gigaToken, nil
}

func (a *App) createStyledDocxFile(items []TCPItem, totalCost int) (string, error) {
	doc, err := document.Open("template.docx")
	if err != nil {
		return "", fmt.Errorf("ошибка открытия template.docx: %w", err)
	}
	orderID := fmt.Sprintf("%d-%d", time.Now().Unix(), rand.Intn(1000))
	replaceAllText(doc, "{document_title}", "Технико-коммерческое предложение")
	replaceAllText(doc, "{order_id}", orderID)
	replaceAllText(doc, "{total_price}", fmt.Sprintf("%d руб.", totalCost))
	var tablePara document.Paragraph
	for _, p := range doc.Paragraphs() {
		var fullParaText strings.Builder
		for _, r := range p.Runs() {
			fullParaText.WriteString(r.Text())
		}
		if strings.Contains(fullParaText.String(), "{components_table}") {
			tablePara = p
			break
		}
	}
	if (tablePara == document.Paragraph{}) {
		return "", fmt.Errorf("плейсхолдер {components_table} не найден в template.docx")
	}
	for _, run := range tablePara.Runs() {
		tablePara.RemoveRun(run)
	}
	table := doc.InsertTableAfter(tablePara)
	tblProps := table.Properties()
	tblProps.SetWidthPercent(100)
	borders := tblProps.Borders()
	borders.SetAll(wml.ST_BorderSingle, color.Auto, measurement.Point)
	headerRow := table.AddRow()
	headers := []string{"Наименование", "Кол-во", "Цена за шт.", "Сумма"}
	for _, h := range headers {
		cell := headerRow.AddCell()
		cell.Properties().SetVerticalAlignment(wml.ST_VerticalJcCenter)
		p := cell.AddParagraph()
		p.Properties().SetAlignment(wml.ST_JcCenter)
		run := p.AddRun()
		run.Properties().SetBold(true)
		run.Properties().SetColor(color.Black)
		run.AddText(h)
	}
	for _, item := range items {
		row := table.AddRow()
		row.AddCell().AddParagraph().AddRun().AddText(item.Name)
		row.AddCell().AddParagraph().AddRun().AddText(strconv.Itoa(item.Quantity))
		row.AddCell().AddParagraph().AddRun().AddText(fmt.Sprintf("%d руб.", item.Price))
		row.AddCell().AddParagraph().AddRun().AddText(fmt.Sprintf("%d руб.", item.Subtotal))
	}
	totalRow := table.AddRow()
	totalLabelCell := totalRow.AddCell()
	totalLabelCell.Properties().SetColumnSpan(3)
	totalLabelPara := totalLabelCell.AddParagraph()
	totalLabelPara.Properties().SetAlignment(wml.ST_JcRight)
	totalLabelRun := totalLabelPara.AddRun()
	totalLabelRun.Properties().SetBold(true)
	totalLabelRun.AddText("Итого:")
	totalValueCell := totalRow.AddCell()
	totalValuePara := totalValueCell.AddParagraph()
	totalValueRun := totalValuePara.AddRun()
	totalValueRun.Properties().SetBold(true)
	totalValueRun.AddText(fmt.Sprintf("%d руб.", totalCost))
	var buf bytes.Buffer
	if err := doc.Save(&buf); err != nil {
		return "", fmt.Errorf("ошибка сохранения docx в буфер: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

func replaceAllText(doc *document.Document, old, new string) {
	for _, p := range doc.Paragraphs() {
		for _, r := range p.Runs() {
			text := r.Text()
			if strings.Contains(text, old) {
				r.ClearContent()
				r.AddText(strings.ReplaceAll(text, old, new))
			}
		}
	}
	for _, h := range doc.Headers() {
		for _, p := range h.Paragraphs() {
			for _, r := range p.Runs() {
				text := r.Text()
				if strings.Contains(text, old) {
					r.ClearContent()
					r.AddText(strings.ReplaceAll(text, old, new))
				}
			}
		}
	}
	for _, f := range doc.Footers() {
		for _, p := range f.Paragraphs() {
			for _, r := range p.Runs() {
				text := r.Text()
				if strings.Contains(text, old) {
					r.ClearContent()
					r.AddText(strings.ReplaceAll(text, old, new))
				}
			}
		}
	}
}

func (a *App) loadProductsFromCache() {
	cachedData, err := os.ReadFile("products.json")
	if err == nil {
		var products []Product
		if json.Unmarshal(cachedData, &products) == nil {
			a.products = products
			a.buildProductMap()
			log.Println("Успешно загружены данные о продуктах из кэша 'products.json'.")
			return
		}
	}
	log.Println("Файл 'products.json' не найден или поврежден. Данные будут загружены из 'materials.csv' при первом запросе.")
}

func (a *App) buildProductMap() {
	a.productMap = make(map[int]Product)
	for _, p := range a.products {
		a.productMap[p.ID] = p
	}
}

func (a *App) ensureDataIsLoaded() error {
	a.dataLoadMutex.Lock()
	defer a.dataLoadMutex.Unlock()
	if len(a.products) > 0 {
		return nil
	}
	log.Println("Данные не загружены, запускаю процесс парсинга 'materials.csv'...")
	rawData, err := os.ReadFile("materials.csv")
	if err != nil {
		return fmt.Errorf("не удалось прочитать 'materials.csv': %w", err)
	}
	prompt := `Ты — сверхточный ассистент по извлечению данных. Твоя задача — преобразовать предоставленный неупорядоченный текст в строгий JSON-массив. Каждая строка текста - отдельный товар. Каждый элемент массива должен быть объектом с полями "name" (строка) и "price" (число).

Правила:
- Извлекай цену как число, убирая "руб." и другие символы.
- Название товара — это всё, что находится до цены.
- Если в строке нет цены, игнорируй её.
- Не добавляй никаких комментариев или текста до и после JSON. Вывод должен быть только валидным JSON-массивом.

Вот текст для обработки:
---
%s
---`
	fullPrompt := fmt.Sprintf(prompt, string(rawData))
	parsedJSON, err := a.callLLMForJSON(fullPrompt)
	if err != nil {
		return fmt.Errorf("ошибка парсинга файла через LLM: %w", err)
	}
	var parsedProducts []ParsedProduct
	if err := json.Unmarshal([]byte(parsedJSON), &parsedProducts); err != nil {
		return fmt.Errorf("LLM вернула невалидный JSON после парсинга: %w. Ответ: %s", err, parsedJSON)
	}
	finalProducts := make([]Product, len(parsedProducts))
	for i, p := range parsedProducts {
		finalProducts[i] = Product{ID: i + 1, Name: p.Name, Price: p.Price}
	}
	finalJSONData, err := json.MarshalIndent(finalProducts, "", "  ")
	if err != nil {
		return fmt.Errorf("ошибка финальной сериализации: %w", err)
	}
	err = os.WriteFile("products.json", finalJSONData, 0644)
	if err != nil {
		return fmt.Errorf("не удалось сохранить кэш в 'products.json': %w", err)
	}
	log.Println("Данные успешно распарсены и сохранены в 'products.json'.")
	a.loadProductsFromCache()
	if len(a.products) == 0 {
		return fmt.Errorf("не удалось загрузить данные в память даже после парсинга")
	}
	return nil
}

func (a *App) saveLogFile(query string, items []TCPItem, totalCost int) error {
	logEntry := LogEntry{
		Query: query,
		Response: FinalLogResponse{
			FoundItems: items,
			TotalCost:  totalCost,
		},
	}
	logData, err := json.MarshalIndent(logEntry, "", "  ")
	if err != nil {
		return fmt.Errorf("не удалось сформировать JSON для лога: %w", err)
	}
	filename := fmt.Sprintf("log_%d.json", time.Now().Unix())
	err = os.WriteFile(filename, logData, 0644)
	if err != nil {
		return fmt.Errorf("не удалось записать лог-файл на диск: %w", err)
	}
	log.Printf("Лог-файл успешно сохранен: %s", filename)
	return nil
}
