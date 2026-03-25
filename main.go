package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/kong"
	openai "github.com/sashabaranov/go-openai"
	"gopkg.in/yaml.v3"
)

//go:embed templates/main.html
var htmlTmpl string

//go:embed static/style.css
var cssContent string

//go:embed static/script.js
var jsContent string

// ─── CLI ──────────────────────────────────────────────────────────────────────

var CLI struct {
	APIBase   string `help:"OpenAI-compatible API host URL (e.g. https://api.example.com)" env:"AI_API_BASE"`
	APIKey    string `help:"API key (optional)" env:"AI_API_KEY"`
	Model     string `help:"Model name" env:"AI_MODEL" default:"gpt-4o-mini"`
	Out       string `help:"Output HTML file" default:"kijiyomu.html"`
	MinScore  int    `help:"Minimum AI score to include (0=include all)" default:"0"`
	HNLimit   int    `help:"Number of HN stories to fetch (used when limit is not set in config)" default:"50"`
	CacheFile string `help:"Cache file for AI scores" default:".kijiyomu_cache.json"`
	Config    string `help:"Feed config YAML file" default:"kijiyomu.yaml"`
}

// ─── Feed config ───────────────────────────────────────────────────────────────

type FeedConfig struct {
	Name      string `yaml:"name"`
	Type      string `yaml:"type"`      // hn, rss, atom, rdf, reddit
	URL       string `yaml:"url"`
	Subreddit string `yaml:"subreddit"` // for type: reddit
	Limit     int    `yaml:"limit"`     // for type: hn (overrides --hn-limit)
}

type Config struct {
	Profile string       `yaml:"profile"`
	Feeds   []FeedConfig `yaml:"feeds"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ─── Data types ───────────────────────────────────────────────────────────────

type Article struct {
	Title     string
	TitleJA   string // 日本語タイトル（英語記事の場合に設定）
	URL       string
	Source    string
	Score     int    // points/bookmarks
	AIScore   int    // 0-100 from LLM
	Reason    string // LLM explanation
	OGImage   string // og:image URL
}

// RSS / Atom

type RSSFeed struct {
	Channel struct {
		Items []RSSItem `xml:"item"`
	} `xml:"channel"`
}

type RSSItem struct {
	Title string `xml:"title"`
	Link  string `xml:"link"`
}

type AtomFeed struct {
	Entries []AtomEntry `xml:"entry"`
}

type AtomEntry struct {
	Title string `xml:"title"`
	Link  struct {
		Href string `xml:"href,attr"`
	} `xml:"link"`
}

// RDF/RSS 1.0（はてなブックマーク等）

type RDFFeed struct {
	Items []RDFItem `xml:"item"`
}

type RDFItem struct {
	Title string `xml:"title"`
	Link  string `xml:"link"`
}

// HN API

type HNStory struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url"`
	Score int    `json:"score"`
}

type AIResult struct {
	Score   int    `json:"score"`
	Reason  string `json:"reason"`
	TitleJA string `json:"title_ja,omitempty"`
}

// ─── Cache ────────────────────────────────────────────────────────────────────

type CacheEntry struct {
	AIScore   int    `json:"ai_score,omitempty"`
	Reason    string `json:"reason,omitempty"`
	TitleJA   string `json:"title_ja,omitempty"`
	OGImage   string `json:"og_image,omitempty"` // og:image URL ("-" = not found)
}

type Cache struct {
	mu      sync.Mutex
	entries map[string]CacheEntry
	path    string
}

func loadCache(path string) *Cache {
	c := &Cache{entries: make(map[string]CacheEntry), path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	if err := json.Unmarshal(data, &c.entries); err != nil {
		log.Printf("[WARN] cache load: %v", err)
	}
	log.Printf("[INFO] cache loaded: %d entries from %s", len(c.entries), path)
	return c
}

func (c *Cache) save() {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := json.MarshalIndent(c.entries, "", "  ")
	if err != nil {
		log.Printf("[WARN] cache marshal: %v", err)
		return
	}
	if err := os.WriteFile(c.path, data, 0644); err != nil {
		log.Printf("[WARN] cache save: %v", err)
	}
}

func (c *Cache) get(u string) (CacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[u]
	return e, ok
}

func (c *Cache) set(u string, entry CacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[u] = entry
}

// ─── Fetchers ─────────────────────────────────────────────────────────────────

func fetchRSS(rawURL, source string) []Article {
	resp, err := http.Get(rawURL)
	if err != nil {
		log.Printf("[WARN] %s: %v", source, err)
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var feed RSSFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		log.Printf("[WARN] %s RSS parse: %v", source, err)
		return nil
	}
	var articles []Article
	for _, item := range feed.Channel.Items {
		link := item.Link
		if link == "" {
			continue
		}
		articles = append(articles, Article{
			Title:  strings.TrimSpace(item.Title),
			URL:    strings.TrimSpace(link),
			Source: source,
		})
	}
	return articles
}

func fetchAtom(rawURL, source string) []Article {
	resp, err := http.Get(rawURL)
	if err != nil {
		log.Printf("[WARN] %s: %v", source, err)
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var feed AtomFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		log.Printf("[WARN] %s Atom parse: %v", source, err)
		return nil
	}
	var articles []Article
	for _, e := range feed.Entries {
		articles = append(articles, Article{
			Title:  strings.TrimSpace(e.Title),
			URL:    strings.TrimSpace(e.Link.Href),
			Source: source,
		})
	}
	return articles
}

func fetchHN(limit int) []Article {
	resp, err := http.Get("https://hacker-news.firebaseio.com/v0/topstories.json")
	if err != nil {
		log.Printf("[WARN] HN topstories: %v", err)
		return nil
	}
	defer resp.Body.Close()
	var ids []int
	json.NewDecoder(resp.Body).Decode(&ids)
	if len(ids) > limit {
		ids = ids[:limit]
	}

	type result struct {
		idx     int
		article Article
	}
	ch := make(chan result, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(idx, id int) {
			defer wg.Done()
			rawURL := fmt.Sprintf("https://hacker-news.firebaseio.com/v0/item/%d.json", id)
			r, err := http.Get(rawURL)
			if err != nil {
				return
			}
			defer r.Body.Close()
			var story HNStory
			json.NewDecoder(r.Body).Decode(&story)
			if story.URL == "" {
				story.URL = fmt.Sprintf("https://news.ycombinator.com/item?id=%d", story.ID)
			}
			ch <- result{idx, Article{
				Title:  story.Title,
				URL:    story.URL,
				Source: "Hacker News",
				Score:  story.Score,
			}}
		}(i, id)
	}
	wg.Wait()
	close(ch)

	articles := make([]Article, 0, len(ids))
	for r := range ch {
		articles = append(articles, r.article)
	}
	return articles
}

func fetchRDF(rawURL, source string) []Article {
	resp, err := http.Get(rawURL)
	if err != nil {
		log.Printf("[WARN] %s: %v", source, err)
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var feed RDFFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		log.Printf("[WARN] %s RDF parse: %v", source, err)
		return nil
	}
	var articles []Article
	for _, item := range feed.Items {
		if item.Link == "" {
			continue
		}
		articles = append(articles, Article{
			Title:  strings.TrimSpace(item.Title),
			URL:    strings.TrimSpace(item.Link),
			Source: source,
		})
	}
	return articles
}


func fetchRedditRSS(sub string) []Article {
	rawURL := fmt.Sprintf("https://www.reddit.com/r/%s/.rss", sub)
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest("GET", rawURL, nil)
	req.Header.Set("User-Agent", "kijiyomu/1.0 (personal RSS reader)")
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[WARN] Reddit r/%s: %v", sub, err)
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var feed AtomFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		log.Printf("[WARN] Reddit r/%s parse: %v", sub, err)
		return nil
	}
	source := "Reddit r/" + sub
	var articles []Article
	for _, e := range feed.Entries {
		href := e.Link.Href
		if !strings.HasPrefix(href, "http") {
			href = "https://www.reddit.com" + href
		}
		articles = append(articles, Article{
			Title:  strings.TrimSpace(e.Title),
			URL:    href,
			Source: source,
		})
	}
	return articles
}


// ─── Deduplication ────────────────────────────────────────────────────────────

// normalizeURL はトラッキング系クエリパラメータを除去して正規化する
func normalizeURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return strings.ToLower(rawURL)
	}
	// utm_* などトラッキング系を除去
	q := u.Query()
	for k := range q {
		kl := strings.ToLower(k)
		if strings.HasPrefix(kl, "utm_") || kl == "ref" || kl == "from" || kl == "source" {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	u.Fragment = ""
	u.Host = strings.ToLower(u.Host)
	result := u.String()
	return strings.TrimRight(result, "/")
}

// deduplicateArticles は同じ URL の記事をまとめ、ソース名を結合する
func deduplicateArticles(articles []Article) []Article {
	type group struct {
		article Article
		sources []string
	}
	seen := make(map[string]int) // normalizedURL → index in groups
	groups := make([]group, 0, len(articles))

	for _, a := range articles {
		key := normalizeURL(a.URL)
		if idx, ok := seen[key]; ok {
			// 既存グループにソースを追加、スコアは高い方を採用
			g := &groups[idx]
			// 重複しないソースのみ追加
			alreadyHas := false
			for _, s := range g.sources {
				if s == a.Source {
					alreadyHas = true
					break
				}
			}
			if !alreadyHas {
				g.sources = append(g.sources, a.Source)
			}
			if a.Score > g.article.Score {
				g.article.Score = a.Score
			}
		} else {
			seen[key] = len(groups)
			groups = append(groups, group{article: a, sources: []string{a.Source}})
		}
	}

	result := make([]Article, len(groups))
	for i, g := range groups {
		a := g.article
		a.Source = strings.Join(g.sources, " / ")
		result[i] = a
	}
	return result
}

// ─── OG image ─────────────────────────────────────────────────────────────────

// fetchOGImage はページの og:image URL を返す。見つからない場合は空文字。
func fetchOGImage(rawURL string) string {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; kijiyomu/1.0)")
	req.Header.Set("Accept", "text/html")

	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	lower := strings.ToLower(string(body))

	// <meta property="og:image" content="..."> を探す（順不同属性に対応）
	searchFor := []string{`property="og:image"`, `property='og:image'`}
	for _, prop := range searchFor {
		idx := strings.Index(lower, prop)
		if idx < 0 {
			continue
		}
		// タグの開始 < を逆方向に探す
		tagStart := strings.LastIndex(lower[:idx], "<")
		if tagStart < 0 {
			continue
		}
		// タグの終了 > を探す
		tagEnd := strings.Index(lower[tagStart:], ">")
		if tagEnd < 0 {
			continue
		}
		tag := string(body[tagStart : tagStart+tagEnd+1])
		tagLower := strings.ToLower(tag)
		ci := strings.Index(tagLower, "content=")
		if ci < 0 {
			continue
		}
		after := tag[ci+8:]
		if len(after) == 0 {
			continue
		}
		quote := after[0]
		if quote != '"' && quote != '\'' {
			continue
		}
		end := strings.IndexByte(after[1:], quote)
		if end < 0 {
			continue
		}
		return strings.TrimSpace(after[1 : end+1])
	}
	return ""
}

// fetchOGImages は全記事の og:image を並列取得する
func fetchOGImages(articles []Article, cache *Cache) []Article {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10)

	for i := range articles {
		// キャッシュ確認
		if e, ok := cache.get(articles[i].URL); ok && e.OGImage != "" {
			if e.OGImage != "-" {
				articles[i].OGImage = e.OGImage
			}
			continue
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			a := &articles[idx]
			img := fetchOGImage(a.URL)
			sentinel := img
			if sentinel == "" {
				sentinel = "-" // 「取得済みだが画像なし」を記録
			}
			a.OGImage = img

			e, _ := cache.get(a.URL)
			e.OGImage = sentinel
			cache.set(a.URL, e)
		}(i)
	}
	wg.Wait()
	return articles
}

// ─── AI client ────────────────────────────────────────────────────────────────

// cleanAPIBase はパスを除いてスキーム+ホストだけを返す
// 例: https://host/v1/chat/completions → https://host
func cleanAPIBase(apiBase string) string {
	if u, err := url.Parse(apiBase); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return apiBase
}

func newAIClient(apiBase, apiKey string) *openai.Client {
	cfg := openai.DefaultConfig(apiKey)
	if apiBase != "" {
		base := cleanAPIBase(apiBase)
		cfg.BaseURL = base + "/v1"
		log.Printf("[INFO] AI base URL: %s", cfg.BaseURL)
	}
	return openai.NewClientWithConfig(cfg)
}

// callAI は OpenAI互換 API に単発リクエストを投げてテキストを返す
func callAI(client *openai.Client, model, system, userMsg string) (string, error) {
	resp, err := client.CreateChatCompletion(context.Background(), openai.ChatCompletionRequest{
		Model: model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: system},
			{Role: openai.ChatMessageRoleUser, Content: userMsg},
		},
	})
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("empty response")
	}
	return strings.TrimSpace(resp.Choices[0].Message.Content), nil
}

// ─── AI scoring ───────────────────────────────────────────────────────────────

func buildSystemPrompt(profile string) string {
	return `あなたは技術記事のレコメンドエンジンです。
以下のユーザープロフィールに基づいて、各記事タイトルに興味スコア(0-100)を付けてください。

## ユーザープロフィール
` + profile + `

## 出力形式
必ずJSON配列のみ返すこと。説明文・前置き・コードブロック記法は不要。
title_ja は元タイトルが英語の場合は日本語に翻訳し、日本語の場合はそのままコピーすること。
[
  {"score": 85, "reason": "Rustの非同期ランタイムに関する記事", "title_ja": "Rustの非同期ランタイム詳解"},
  {"score": 10, "reason": "政治ニュースのため", "title_ja": "〇〇の政治情勢"}
]`
}

func scoreArticlesWithAI(articles []Article, client *openai.Client, model string, batchSize int, cache *Cache, systemPrompt string) []Article {
	if client == nil {
		log.Println("[INFO] AI client not configured, skipping AI scoring")
		return articles
	}

	// キャッシュ適用・未スコアのインデックスを収集
	uncached := make([]int, 0, len(articles))
	for i := range articles {
		if e, ok := cache.get(articles[i].URL); ok && e.AIScore > 0 {
			articles[i].AIScore = e.AIScore
			articles[i].Reason = e.Reason
			articles[i].TitleJA = e.TitleJA
		} else {
			uncached = append(uncached, i)
		}
	}
	log.Printf("  scoring %d articles (cached: %d)", len(uncached), len(articles)-len(uncached))

	for bStart := 0; bStart < len(uncached); bStart += batchSize {
		bEnd := bStart + batchSize
		if bEnd > len(uncached) {
			bEnd = len(uncached)
		}
		batch := uncached[bStart:bEnd]

		var titlesBuilder strings.Builder
		for i, idx := range batch {
			fmt.Fprintf(&titlesBuilder, "%d. %s\n", i+1, articles[idx].Title)
		}

		content, err := callAI(client, model, systemPrompt, "以下の記事タイトルを評価してください:\n\n"+titlesBuilder.String())
		if err != nil {
			log.Printf("[WARN] AI API error: %v", err)
			continue
		}

		content = strings.TrimSpace(content)
		content = strings.TrimPrefix(content, "```json")
		content = strings.TrimPrefix(content, "```")
		content = strings.TrimSuffix(content, "```")

		var results []AIResult
		if err := json.Unmarshal([]byte(content), &results); err != nil {
			log.Printf("[WARN] AI JSON parse error: %v\ncontent: %s", err, content)
			continue
		}
		for i, r := range results {
			if i >= len(batch) {
				break
			}
			idx := batch[i]
			articles[idx].AIScore = r.Score
			articles[idx].Reason = r.Reason
			articles[idx].TitleJA = r.TitleJA
			e, _ := cache.get(articles[idx].URL)
			e.AIScore = r.Score
			e.Reason = r.Reason
			e.TitleJA = r.TitleJA
			cache.set(articles[idx].URL, e)
		}

		time.Sleep(500 * time.Millisecond)
	}
	return articles
}

// ─── HTML output ──────────────────────────────────────────────────────────────

// ─── Template helpers ─────────────────────────────────────────────────────────

func scoreClass(score int) string {
	switch {
	case score >= 70:
		return "score-high"
	case score >= 40:
		return "score-mid"
	case score > 0:
		return "score-low"
	default:
		return "score-none"
	}
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	kong.Parse(&CLI)

	cache := loadCache(CLI.CacheFile)

	var aiClient *openai.Client
	if CLI.APIBase != "" {
		aiClient = newAIClient(CLI.APIBase, CLI.APIKey)
	}

	log.Println("Fetching articles...")

	type fetchJob struct {
		name string
		fn   func() []Article
	}

	var jobs []fetchJob
	var feedCfg *Config
	if cfg, err := loadConfig(CLI.Config); err != nil {
		log.Printf("[WARN] config load (%s): %v", CLI.Config, err)
	} else {
		feedCfg = cfg
		for _, f := range feedCfg.Feeds {
			limit := f.Limit
			if limit == 0 {
				limit = CLI.HNLimit
			}
			var fn func() []Article
			switch f.Type {
			case "hn":
				lim := limit
				fn = func() []Article { return fetchHN(lim) }
			case "rss":
				fn = func() []Article { return fetchRSS(f.URL, f.Name) }
			case "atom":
				fn = func() []Article { return fetchAtom(f.URL, f.Name) }
			case "rdf":
				fn = func() []Article { return fetchRDF(f.URL, f.Name) }
			case "reddit":
				fn = func() []Article { return fetchRedditRSS(f.Subreddit) }
			default:
				log.Printf("[WARN] unknown feed type %q for %q", f.Type, f.Name)
				continue
			}
			jobs = append(jobs, fetchJob{name: f.Name, fn: fn})
		}
	}

	var mu sync.Mutex
	var allArticles []Article
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		go func(j fetchJob) {
			defer wg.Done()
			arts := j.fn()
			log.Printf("  [%s] %d articles", j.name, len(arts))
			mu.Lock()
			allArticles = append(allArticles, arts...)
			mu.Unlock()
		}(job)
	}
	wg.Wait()
	log.Printf("Total fetched: %d articles", len(allArticles))

	// 同一URL記事を統合
	before := len(allArticles)
	allArticles = deduplicateArticles(allArticles)
	log.Printf("After dedup: %d articles (removed %d duplicates)", len(allArticles), before-len(allArticles))

	// OG image fetch (all articles, cached)
	log.Println("Fetching OG images...")
	allArticles = fetchOGImages(allArticles, cache)

	// AI scoring
	if aiClient != nil {
		log.Printf("Scoring with AI (model: %s)...", CLI.Model)
		var profile string
		if feedCfg != nil {
			profile = strings.TrimSpace(feedCfg.Profile)
		}
		allArticles = scoreArticlesWithAI(allArticles, aiClient, CLI.Model, 20, cache, buildSystemPrompt(profile))
		sort.Slice(allArticles, func(i, j int) bool {
			return allArticles[i].AIScore > allArticles[j].AIScore
		})
		if CLI.MinScore > 0 {
			filtered := allArticles[:0]
			for _, a := range allArticles {
				if a.AIScore >= CLI.MinScore {
					filtered = append(filtered, a)
				}
			}
			allArticles = filtered
		}
	}

	cache.save()

	// collect unique sources for filter buttons
	sourceSet := map[string]bool{}
	for _, a := range allArticles {
		sourceSet[a.Source] = true
	}
	var sources []string
	for s := range sourceSet {
		sources = append(sources, s)
	}
	sort.Strings(sources)

	// render HTML
	funcMap := template.FuncMap{
		"scoreClass": scoreClass,
	}
	tmpl := template.Must(template.New("feed").Funcs(funcMap).Parse(htmlTmpl))

	data := struct {
		Date     string
		Articles []Article
		Sources  []string
		CSS      template.CSS
		JS       template.JS
	}{
		Date:     time.Now().Format("2006-01-02 15:04"),
		Articles: allArticles,
		Sources:  sources,
		CSS:      template.CSS(cssContent),
		JS:       template.JS(jsContent),
	}

	f, err := os.Create(CLI.Out)
	if err != nil {
		log.Fatalf("create output: %v", err)
	}
	defer f.Close()
	if err := tmpl.Execute(f, data); err != nil {
		log.Fatalf("render template: %v", err)
	}
	log.Printf("Written: %s (%d articles)", CLI.Out, len(allArticles))
}
