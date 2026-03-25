package main

import (
	"context"
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

const htmlTmpl = `<!DOCTYPE html>
<html lang="ja">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>KijiYomu - {{.Date}}</title>
<style>
  :root {
    --bg: #0d0f14; --surface: #161820; --surface2: #1e2030; --border: #252736;
    --text: #e2e8f0; --muted: #556070; --accent: #6366f1;
    --green: #22c55e; --yellow: #eab308; --red: #ef4444;
  }
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { background: var(--bg); color: var(--text); font-family: -apple-system, 'Helvetica Neue', sans-serif; font-size: 14px; line-height: 1.5; }
  .wrap { max-width: 1200px; margin: 0 auto; }

  /* ── header ── */
  header { padding: 16px 24px; border-bottom: 1px solid var(--border); display: flex; align-items: center; gap: 12px; }
  header h1 { font-size: 17px; font-weight: 700; color: var(--accent); }
  header span { color: var(--muted); font-size: 12px; }

  /* ── controls ── */
  .controls { padding: 10px 24px; display: flex; gap: 8px; flex-wrap: wrap; align-items: center; border-bottom: 1px solid var(--border); }
  .filter-btn { padding: 4px 12px; border-radius: 16px; border: 1px solid var(--border); background: transparent; color: var(--muted); cursor: pointer; font-size: 12px; transition: all .12s; white-space: nowrap; }
  .filter-btn:hover, .filter-btn.active { background: var(--accent); border-color: var(--accent); color: #fff; }
  .threshold { display: flex; align-items: center; gap: 6px; margin-left: auto; color: var(--muted); font-size: 12px; flex-shrink: 0; }
  .threshold input { width: 52px; padding: 3px 6px; border-radius: 6px; border: 1px solid var(--border); background: var(--surface); color: var(--text); font-size: 12px; }
  .stats { padding: 6px 24px; color: var(--muted); font-size: 11px; border-bottom: 1px solid var(--border); }

  /* ── card grid ── */
  .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(280px, 1fr)); gap: 1px; background: var(--border); }

  /* ── card ── */
  .card { background: var(--surface); display: flex; flex-direction: column; cursor: default; transition: background .12s; }
  .card:hover { background: var(--surface2); }
  .card.selected { background: var(--surface2); outline: 2px solid var(--accent); outline-offset: -2px; position: relative; z-index: 1; }
  .card.read { opacity: 0.38; }
  .card.read .card-thumb img { filter: grayscale(1); }

  .card-thumb { width: 100%; aspect-ratio: 16/9; overflow: hidden; background: var(--surface2); flex-shrink: 0; }
  .card-thumb img { width: 100%; height: 100%; object-fit: cover; display: block; transition: transform .2s; }
  .card:hover .card-thumb img { transform: scale(1.03); }
  .card-no-thumb { width: 100%; aspect-ratio: 16/9; background: linear-gradient(135deg, var(--surface2), var(--border)); display: flex; align-items: center; justify-content: center; }
  .card-no-thumb .score-big { font-size: 28px; font-weight: 800; }

  .card-body { padding: 12px 14px; display: flex; flex-direction: column; gap: 6px; flex: 1; }

  .card-title { font-size: 13px; font-weight: 600; line-height: 1.4; color: var(--text); }
  .card-title a { color: inherit; text-decoration: none; }
  .card-title a:hover { color: var(--accent); }
  .card-title-orig { font-size: 10px; color: var(--muted); margin-top: 2px; line-height: 1.3; }

  .card-meta { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; }
  .score-badge { font-size: 11px; font-weight: 700; padding: 1px 7px; border-radius: 10px; flex-shrink: 0; }
  .score-high { background: rgba(34,197,94,.18); color: var(--green); }
  .score-mid  { background: rgba(234,179,8,.18);  color: var(--yellow); }
  .score-low  { background: rgba(239,68,68,.12);  color: var(--red); }
  .score-none { background: rgba(100,116,139,.12); color: var(--muted); }
  .source-tag { font-size: 10px; background: rgba(99,102,241,.15); color: var(--accent); padding: 1px 7px; border-radius: 10px; }
  .hn-score { font-size: 10px; color: var(--muted); }


  .hidden { display: none !important; }

  /* ── mark all read bar ── */
  .mark-all-bar { padding: 40px 24px; display: flex; justify-content: center; }
  .mark-all-btn { padding: 16px 56px; font-size: 16px; font-weight: 700; background: var(--accent); color: #fff; border: none; border-radius: 14px; cursor: pointer; letter-spacing: .03em; transition: opacity .12s, transform .1s; }
  .mark-all-btn:hover { opacity: 0.85; transform: translateY(-1px); }
  .mark-all-btn:active { transform: translateY(0); }
</style>
</head>
<body>
<div class="wrap">
<header>
  <h1>⚡ KijiYomu</h1>
  <span>{{.Date}} — {{len .Articles}} 件</span>
</header>
<div class="controls">
  <button class="filter-btn active" onclick="filterSource('all')">All</button>
  {{range .Sources}}<button class="filter-btn" onclick="filterSource('{{.}}')">{{.}}</button>{{end}}
  <button id="read-toggle" class="filter-btn" onclick="toggleShowRead()">既読も表示</button>
  <div class="threshold">
    スコア: <input type="number" id="threshold" value="0" min="0" max="100" onchange="applyThreshold()">+
  </div>
</div>
<div class="stats">表示中: <span id="visible-count">{{len .Articles}}</span> 件</div>
<div class="grid" id="articles">
{{range .Articles}}
<div class="card" data-source="{{.Source}}" data-score="{{.AIScore}}" data-url="{{.URL}}">
  {{if .OGImage}}
  <div class="card-thumb"><img src="{{.OGImage}}" alt="" loading="lazy" onerror="this.closest('.card-thumb').replaceWith(Object.assign(document.createElement('div'),{className:'card-no-thumb',innerHTML:'<span class=\'score-big {{scoreClass .AIScore}}\'>{{if gt .AIScore 0}}{{.AIScore}}{{else}}?{{end}}</span>'}))"></div>
  {{else}}
  <div class="card-no-thumb"><span class="score-big {{scoreClass .AIScore}}">{{if gt .AIScore 0}}{{.AIScore}}{{else}}?{{end}}</span></div>
  {{end}}
  <div class="card-body">
    <div class="card-title"><a href="{{.URL}}" target="_blank" rel="noopener">{{if .TitleJA}}{{.TitleJA}}{{else}}{{.Title}}{{end}}</a></div>
    {{if .TitleJA}}<div class="card-title-orig">{{.Title}}</div>{{end}}
    <div class="card-meta">
      {{if gt .AIScore 0}}<span class="score-badge {{scoreClass .AIScore}}">{{.AIScore}}</span>{{end}}
      <span class="source-tag">{{.Source}}</span>
      {{if gt .Score 0}}<span class="hn-score">▲{{.Score}}</span>{{end}}
    </div>
  </div>
</div>
{{end}}
</div>
<div id="mark-all-bar" class="mark-all-bar">
  <button class="mark-all-btn" onclick="markAllAsRead()">すべて既読にする</button>
</div>
</div>
<script>
// ── read state (localStorage) ────────────────────────────────────────────────
const READ_KEY = 'kijiyomu_read';
const READ_TTL = 7 * 24 * 60 * 60 * 1000; // 7日

function getReadMap() {
  try { return JSON.parse(localStorage.getItem(READ_KEY) || '{}'); }
  catch { return {}; }
}
function getReadSet() {
  const now = Date.now(), map = getReadMap();
  return new Set(Object.entries(map).filter(([, ts]) => now - ts < READ_TTL).map(([u]) => u));
}
function saveRead(url) {
  const now = Date.now(), map = getReadMap();
  map[url] = now;
  // 期限切れを削除
  for (const [u, ts] of Object.entries(map)) {
    if (now - ts >= READ_TTL) delete map[u];
  }
  localStorage.setItem(READ_KEY, JSON.stringify(map));
}

// ページ読み込み時: 既読をグレーにして末尾へ移動、デフォルトは非表示
let showRead = false;

(function initRead() {
  const read = getReadSet();
  const grid = document.getElementById('articles');
  const cards = Array.from(grid.querySelectorAll('.card'));
  const readCards = [];
  cards.forEach(card => {
    if (read.has(card.dataset.url)) {
      card.classList.add('read', 'read-hidden');
      readCards.push(card);
    }
  });
  readCards.forEach(card => grid.appendChild(card));
  updateReadBtn();
  updateMarkAllBar();
})();

function updateReadBtn() {
  const read = getReadSet();
  const btn = document.getElementById('read-toggle');
  if (!btn) return;
  const count = document.querySelectorAll('.card.read').length;
  btn.textContent = showRead ? '既読を隠す (' + count + ')' : '既読も表示 (' + count + ')';
  btn.classList.toggle('active', showRead);
}

function toggleShowRead() {
  showRead = !showRead;
  document.querySelectorAll('.card.read').forEach(c => {
    c.classList.toggle('read-hidden', !showRead);
  });
  updateReadBtn();
  applyThreshold(); // 表示件数を再計算
}

function updateMarkAllBar() {
  const bar = document.getElementById('mark-all-bar');
  if (!bar) return;
  const hasUnread = document.querySelector('.card:not(.read)') !== null;
  bar.classList.toggle('hidden', !hasUnread);
}

function markAllAsRead() {
  document.querySelectorAll('.card:not(.read)').forEach(card => {
    card.classList.add('read');
    if (!showRead) card.classList.add('read-hidden');
    saveRead(card.dataset.url);
  });
  updateMarkAllBar();
  updateReadBtn();
  applyThreshold();
}

// 一度ビューポートに入ったカードが画面外に出たら既読に
const seenCards = new Set();
const readObserver = new IntersectionObserver(entries => {
  entries.forEach(entry => {
    const card = entry.target;
    if (card.classList.contains('read')) return;
    if (entry.isIntersecting) {
      seenCards.add(card);
    } else if (seenCards.has(card)) {
      seenCards.delete(card);
      card.classList.add('read'); // グレーにするだけ。今セッションは消さない
      saveRead(card.dataset.url);
      updateReadBtn();
      updateMarkAllBar();
    }
  });
}, { threshold: 0.1 });

document.querySelectorAll('.card[data-url]').forEach(c => readObserver.observe(c));

// ── keyboard navigation ──────────────────────────────────────────────────────
let selectedIdx = -1;

function visibleCards() {
  return Array.from(document.querySelectorAll('.card:not(.hidden)'));
}

function colCount() {
  const cards = visibleCards();
  if (cards.length < 2) return 1;
  const top0 = cards[0].getBoundingClientRect().top;
  let n = 1;
  for (let i = 1; i < cards.length; i++) {
    if (Math.abs(cards[i].getBoundingClientRect().top - top0) < 4) n++;
    else break;
  }
  return n;
}

function selectCard(idx) {
  const cards = visibleCards();
  if (!cards.length) return;
  // clamp
  idx = Math.max(0, Math.min(idx, cards.length - 1));
  if (selectedIdx >= 0 && selectedIdx < cards.length)
    cards[selectedIdx].classList.remove('selected');
  selectedIdx = idx;
  const card = cards[selectedIdx];
  card.classList.add('selected');
  card.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
}

document.addEventListener('keydown', e => {
  if (e.target.tagName === 'INPUT' || e.metaKey || e.ctrlKey) return;
  const cards = visibleCards();
  if (!cards.length) return;
  const cols = colCount();
  switch (e.key) {
    case 'j': case 'J':
      e.preventDefault();
      selectCard(selectedIdx < 0 ? 0 : selectedIdx + cols); break;
    case 'k': case 'K':
      e.preventDefault();
      selectCard(selectedIdx < 0 ? 0 : selectedIdx - cols); break;
    case 'h': case 'H':
      e.preventDefault();
      selectCard(selectedIdx < 0 ? 0 : selectedIdx - 1); break;
    case 'l': case 'L':
      e.preventDefault();
      selectCard(selectedIdx < 0 ? 0 : selectedIdx + 1); break;
    case 'Enter':
      if (selectedIdx >= 0 && selectedIdx < cards.length) {
        const a = cards[selectedIdx].querySelector('.card-title a');
        if (a) window.open(a.href, '_blank', 'noopener');
      }
      break;
  }
});

// フィルター変更時に選択リセット
function resetSelection() { selectedIdx = -1; }

// ── source filter / threshold ────────────────────────────────────────────────
let currentSource = 'all';
function filterSource(src) {
  currentSource = src;
  resetSelection();
  document.querySelectorAll('.filter-btn').forEach(b => {
    b.classList.toggle('active', b.textContent.trim() === (src === 'all' ? 'All' : src));
  });
  applyThreshold();
}
function applyThreshold() {
  resetSelection();
  const th = parseInt(document.getElementById('threshold').value) || 0;
  let count = 0;
  document.querySelectorAll('.card').forEach(card => {
    const srcOk = currentSource === 'all' || card.dataset.source === currentSource;
    const scoreOk = (parseInt(card.dataset.score) || 0) >= th;
    const readOk = showRead || !card.classList.contains('read-hidden');
    const show = srcOk && scoreOk && readOk;
    card.classList.toggle('hidden', !show);
    if (show) count++;
  });
  document.getElementById('visible-count').textContent = count;
}
</script>
</body>
</html>`

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
	}{
		Date:     time.Now().Format("2006-01-02 15:04"),
		Articles: allArticles,
		Sources:  sources,
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
