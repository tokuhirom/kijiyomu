# KijiYomu

複数の技術系フィードから記事を収集し、AIがあなたの興味に合わせてスコアリングして HTML カードビューで出力する CLI ツール。

## セットアップ

```bash
go build -o kijiyomu .
```

dotenvx を使う場合:

```bash
# .env を作成
cp .env.example .env  # または手動で作成

dotenvx run -- go run main.go
```

## .env の設定

```env
AI_API_BASE=https://api.ai.sakura.ad.jp/v1/chat/completions
AI_API_KEY=your-api-key
AI_MODEL=gpt-oss-120b
```

## 使い方

```bash
# AI スコアリングあり（推奨）
dotenvx run -- ./kijiyomu

# スコア 40 以上のみ表示
dotenvx run -- ./kijiyomu --min-score 40

# HN の取得件数を増やす
dotenvx run -- ./kijiyomu --hn-limit 100
```

## オプション一覧

| フラグ | 環境変数 | デフォルト | 説明 |
|---|---|---|---|
| `--api-base` | `AI_API_BASE` | (なし) | OpenAI互換 API の URL |
| `--api-key` | `AI_API_KEY` | (なし) | API キー |
| `--model` | `AI_MODEL` | `gpt-4o-mini` | モデル名 |
| `--out` | | `kijiyomu.html` | 出力 HTML ファイル名 |
| `--min-score` | | `0` | AI スコアの下限（0=フィルタなし） |
| `--hn-limit` | | `50` | HN から取得するストーリー数（config で未指定の場合） |
| `--cache-file` | | `.kijiyomu_cache.json` | キャッシュファイルのパス |
| `--config` | | `kijiyomu.yaml` | フィードソース設定ファイル |

## フィードソースの設定（kijiyomu.yaml）

`kijiyomu.yaml` でフィードソースを自由に追加・削除できます。

```yaml
feeds:
  - name: Hacker News
    type: hn
    limit: 50          # 省略すると --hn-limit の値を使用

  - name: はてなブックマーク
    type: rdf
    url: https://b.hatena.ne.jp/hotentry/it.rss

  - name: Zenn
    type: rss
    url: https://zenn.dev/feed

  - name: Qiita
    type: atom
    url: https://qiita.com/popular-items/feed

  - name: Reddit r/rust
    type: reddit
    subreddit: rust

  - name: Google さくらインターネット
    type: rss
    url: 'https://news.google.com/rss/...'
```

### type の種類

| type | 説明 |
|---|---|
| `hn` | Hacker News（Firebase API） |
| `rss` | RSS 2.0 |
| `atom` | Atom |
| `rdf` | RDF/RSS 1.0（はてなブックマーク等） |
| `reddit` | Reddit JSON API（`subreddit` フィールドが必要） |

## HTML の機能

- **カードグリッド表示** — OG イメージ付きのカード形式
- **ソース別フィルタ** — ボタンでソースを絞り込み
- **スコアフィルタ** — AI スコアの下限をリアルタイムで変更
- **既読管理** — スクロールで通り過ぎた記事は自動でグレーアウト、リロード後は下に移動（localStorage、7日で自動削除）
- **キーボードナビ** — `H`/`J`/`K`/`L` で移動、`Enter` で開く
- **重複排除** — 同一 URL の記事は複数ソースをまとめて表示

## GitHub Actions による自動実行

`.github/workflows/kijiyomu.yml` で 6 時間ごとに実行し、GitHub Pages へデプロイします。

リポジトリの **Settings → Secrets** に以下を登録してください:

| シークレット名 | 内容 |
|---|---|
| `AI_API_BASE` | API の URL |
| `AI_API_KEY` | API キー |
| `AI_MODEL` | モデル名 |

キャッシュ（AI スコア・OG イメージ）は `actions/cache` で runs をまたいで保持されます。

## インタレストプロフィールの変更

`kijiyomu.yaml` の `profile` フィールドを編集してください。

```yaml
profile: |
  - 言語/技術: Rust, Go, TypeScript, Python
  - 分野: システムプログラミング, LLM/AIエージェント, WebAssembly, クラウドインフラ
  - 趣味: Minecraft, roguelike, ピクセルアート
  - 関心低め: 政治, 芸能, スポーツ, ファッション
```
