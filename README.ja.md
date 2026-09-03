# gem-usage-lens

[gem-agent](https://github.com/nlink-jp/gem-agent)（Vertex AI Gemini）のセッション
transcript からトークン使用量とコストを集計する CLI。

gem-agent はモデル呼び出しごとに会計レコード（prompt / output / thinking / cached の
トークン数、モデル、どの処理が消費したか）を transcript に書きます。Vertex AI は
金額を返さないので、`gem-usage-lens` がそのレコードを読み、Vertex AI の定価を掛け、
ローカルの SQLite に蓄積して、日・セッション・プロジェクト・モデル・呼び出し種別で
集計します。**暦月の予算**とペース予測も持ちます。

[claude-usage-lens](https://github.com/nlink-jp/claude-usage-lens) の対です。
メニューバーアプリ [gem-usage-lens-gui](https://github.com/nlink-jp/gem-usage-lens-gui)
が同じデータを描画します。

> コストは transcript のトークン数に Vertex AI の**定価を掛けた換算値（notional）**です。
> 請求額ではありません（コミットメント割引・クレジット・端数処理は考慮しません）。
> 実際の請求は Cloud Billing を参照してください。

> macOS 版は **Developer ID 署名 + Apple notarization 済み**です。Linux / Windows 版は
> 未署名の experimental です。

## インストール

Homebrew（macOS, Apple silicon）:

```bash
brew install nlink-jp/tap/gem-usage-lens
```

または [Releases](https://github.com/nlink-jp/gem-usage-lens/releases) のアーカイブを
展開し、`gem-usage-lens` を PATH に置きます。

前提: gem-agent の transcript が `~/.local/state/gem-agent/sessions`（または
`GEMAGENT_STATE_DIR` 配下）にあること。認証情報・ネットワークは不要です。

## クイックスタート

```bash
gem-usage-lens doctor            # transcript / config / store の解決先
gem-usage-lens ingest            # 追記分だけ取り込み（増分・冪等）
gem-usage-lens report --since 7d # 直近 7 日を日別に
gem-usage-lens budget --limit-usd 100
```

## コマンド

| コマンド | 内容 |
|----------|------|
| `ingest` | 前回以降に追記されたバイトだけを読み、store へ upsert。いつ実行しても安全。 |
| `report` | store を集計。`--since` / `--until` / `--group-by` / `--source` / `--model` / `--project` / `--sort` / `--top` / `--dense` / `--summary` / `--compare` / `--tz` / `--json`。 |
| `budget` | 暦月予算の状態: 消費・残量・警告状態・ペース予測。`--limit-usd` / `--limit-tokens` / `--warn` / `--critical` / `--tz` / `--json`。 |
| `sessions` | セッションごとに 1 行（`--sort cost --top 10`）。 |
| `models` | 単価表（検証日つき）と、config 由来のエントリ。 |
| `reprice` | 単価変更後（config または新ビルド）に蓄積済みコストを再計算。`--dry-run` で確認。 |
| `verify` | 全 transcript の会計チェックサム（`prompt + output + thoughts + tool prompt == total`）を検査し、gem-agent ADR-0057 以前のファイルを列挙。 |
| `doctor` | sessions root・config パス（無ければ探索した全パス）・store パスを表示。 |
| `watch` | ポーリングで継続取り込みし、コスト差分をライブ表示（`--interval 5s`）。 |
| `daemon` | `ingest` を定期実行する launchd ジョブの `install` / `uninstall` / `status`（macOS）。再ビルドされる開発ビルドではなく、インストール済みバイナリ（Homebrew / PATH）から登録してください。 |

`--since` は日付（`2026-09-01`）、日時（`2026-09-01T09:00`）、RFC 3339、相対
（`7d`）、`today`、`month`（当月 1 日）を受け付けます。日・月の境界は `--tz`
（既定 local）に従います。

### report

```
$ gem-usage-lens report --since 30d --group-by source
KEY                   RECORDS  PROMPT    CACHED    OUTPUT  THOUGHTS  TOTAL     COST(USD)
main*                 924      93013003  75522739  331618  116003    93460624  $20.4605
risk                  102      117221    0         3151    15199     135571    $0.1567
web_search            5        257       0         2968    1947      5172      $0.1936
...
```

- **CACHED** は PROMPT のうちキャッシュから供給された分（内数。加算ではない）。
- **THOUGHTS** は思考トークン。出力単価で課金されます。
- **TOOL** は `toolUsePromptTokenCount`: 組み込みツール（検索グラウンディング・URL
  context）の結果がモデルへ入力として戻された分。gem-agent はこのバケットを書きませんが
  API の `total` には含まれるので、`gem-usage-lens` が残差として導出し入力単価で課金します。
- **TOTAL** は prompt + output + thoughts + tool prompt = 課金対象トークン数。
- `*` は gem-agent ADR-0057（2026-08-30）以前の transcript 由来のレコードを含む行。
  当時のファイルは risk / compaction の消費を記録していないので、その合計は下限値です。

`--summary` は稼働日数・日平均・ピーク日・30 日換算に加え、**`unpriced_records`**
（取り込み時に単価表に無かったモデルの $0 行数）を出します。[単価](#単価) を参照。

### budget

```
$ gem-usage-lens budget --limit-usd 100
month:   2026-09-01 → 2026-10-01 (Local)   elapsed 7%
cost:    $1.60 used / $100.00 limit   2% used · $98.40 left (98%)   [normal]
         pace: on pace for $23.81 (24%) by the reset
tokens:  6.2M used (no limit set — pass --limit-usd/--limit-tokens or set [budget] in config.toml)
```

窓は `--tz` での暦月で、毎月 1 日 0:00 にリセットされます。USD と課金トークンの
両基準を表示します。ペース行は経過割合から線形に外挿し、月の先頭 5% では
1 セッションで外挿しないよう「too early」と述べます。上限は config の `[budget]`
が既定、フラグが上書きです。

## 単価

Vertex AI の **global** エンドポイントにおける USD / 100 万トークン。公式料金
ページから取り、検証日を刻んでいます（`gem-usage-lens models` が表示）。1 呼び出しの
計算:

```
(prompt − cached) × input  +  cached × input × 0.1  +  tool prompt × input  +  (output + thoughts) × output
+ $0.035（web_search の場合。Google 検索グラウンディング $35 / 1,000 件）
× 1.1（セッションの location が "global" 以外の場合）
```

キャッシュ保存料（明示キャッシュ）やバッチ / 優先度ティアは対象外です（gem-agent は
使いません）。

単価表に無いモデルは **$0** になり、それをあらゆる面で明示します: `ingest` /
`reprice` の stderr 警告、`report --summary` の `unpriced_records`（モデル別）、
GUI のバッジ。リリースを待たずに直すには `config.toml` で単価を書いて `reprice`:

```toml
[pricing.models."gemini-4-flash"]
input_per_mtok  = 1.0
output_per_mtok = 5.0
```

書いたフィールドだけが上書きされ、残りは継承します（キャッシュ倍率 0.1、
グラウンディング $0.035、非 global 1.1）。全フィールドは
[config.example.toml](config.example.toml) を参照。

## 設定

`config.toml` は次の順で探します:

1. `$XDG_CONFIG_HOME/gem-usage-lens/config.toml`（変数が設定されているとき）
2. `~/.config/gem-usage-lens/config.toml`
3. `~/Library/Application Support/gem-usage-lens/config.toml`（macOS）

すべて任意。未知のキーはエラーです。セクション: `[sources] sessions_root`、
`[pricing.models."<id>"]`、`[budget]`。`doctor` が読み込んだファイルを表示します。

store は macOS では `~/Library/Application Support/gem-usage-lens/usage.db`
（それ以外は `$XDG_DATA_HOME/gem-usage-lens`）、所有者のみ読み書き可。`ingest` は
行を削除しないので、transcript が消えても履歴は残ります。

## データソース

| 項目 | 値 |
|------|-----|
| sessions root | `$GEMAGENT_STATE_DIR/sessions`、無ければ `~/.local/state/gem-agent/sessions` |
| ファイル | `**/*.jsonl`（1 セッション 1 ファイル。resume は同じファイルに追記） |
| レコード | `{"kind":"usage","data":{"source","model","prompt","output","thoughts","cached","total"}}` と、ヘッダの `model` / `project` / `location` |

レコードは（sessions root からの相対）ファイルパス + バイトオフセットで識別するので、
`ingest` を何度実行しても二重計上せず、GUI・`watch`・daemon が同時に取り込んでも
安全です。書きかけの行は次回に回します。sessions root を変更すると全ファイルの
キーが変わるので、その後は同じ transcript を二重に取り込まず `usage.db` を削除してください。

gem-agent v0.55（ADR-0057、2026-08-30）以前の transcript は main ループ分しか無く `source` /
`model` も無いので、ヘッダから補い `partial` として印を付けます。`verify` が一覧します。

## JSON

`report --json`、`report --summary --json`、`budget --json`、`sessions --json`、
`models --json`、`verify --json` は安定した機械可読出力です（GUI は前 3 つを使用）。
時刻は秒精度の RFC 3339 です。

## ビルド

```bash
make build      # → dist/gem-usage-lens
make test
make vet        # host + linux + windows
```

## ドキュメント

- [RFP（設計）](docs/ja/gem-usage-lens-rfp.ja.md) · [English](docs/en/gem-usage-lens-rfp.md)
- [gem-agent ADR-0057 — 会計レコード](https://github.com/nlink-jp/gem-agent/blob/main/docs/ja/adr/0057-usage-accounting-records.ja.md)

## ライセンス

MIT
