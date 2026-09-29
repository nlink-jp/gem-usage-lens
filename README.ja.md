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
| `sessions` | セッションごとに 1 行。最初と最後のモデル呼び出し時刻つきで時系列順（既定 `--sort time` なので `--top N` 単独は古い方から N 件。大きい順は `--sort cost --top 10`）。`--tz` / `--json`。 |
| `models` | 単価表（価格の期間ごとに 1 行、開始の瞬間つき）と検証日、config 由来のエントリ。 |
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
  context）の結果がモデルへ入力として戻された分で、入力単価で課金されます。gem-agent は
  v0.62.0 から `tool_prompt` として書きます。それ以前の transcript にはキーが無いので、
  `gem-usage-lens` が `total` の残差として導出します（`verify` では `TOOL-DERIVED`）。
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
+ $0.014（web_search の場合。Google 検索グラウンディング $14 / 1,000 クエリ）
× 1.1（セッションの location が "global" 以外の場合）
```

キャッシュ保存料（明示キャッシュ）やバッチ / 優先度ティアは対象外です（gem-agent は
使いません）。グラウンディングは「グラウンディング クエリ」単位の課金で、1 回の
プロンプトが複数クエリを発行することがあります。本ツールは `web_search` 1 回につき
1 クエリ分を加算するので、この行は下限です。Gemini 3 系で合算される月 5,000 クエリの
無料枠も対象外です（請求額ではなく定価換算のため）。

**各呼び出しは、その時点で有効だった単価で計算します。** モデルの単価はある日付で
変わることがあります — Google は 3.8 / 3.7 / 3.6 Flash を、現行の導入価格
$0.75 / $3.75 から 2027-01-01 に $1.50 / $7.50 へ上げると掲載しています — そのため
単価表はモデルごとの単価を、ある瞬間から始まる期間として持ちます（`models` の
`FROM` 列）。表に載せるのは有効な単価だけです。予定された変更は実施された後に、
その日付の米国太平洋時間の午前 0 時から始まる新しい期間として追加し、`reprice` が
その瞬間以降の呼び出しだけを直します（それより前には触れません）。その更新が出荷
されるまで、価格変更後の呼び出しは旧単価で記録されます。

単価表に無いモデルは **$0** になり、それをあらゆる面で明示します: `ingest` /
`reprice` の stderr 警告、`report --summary` の `unpriced_records`（モデル別）、
GUI のバッジ。リリースを待たずに直すには `config.toml` で単価を書いて `reprice`:

```toml
[pricing.models."gemini-4-flash"]
input_per_mtok  = 1.0
output_per_mtok = 5.0
```

書いたフィールドだけが上書きされ、残りは継承します（キャッシュ倍率 0.1、
グラウンディング $0.014、非 global 1.1）。config には日付がありません: 書いた
フィールドは全日付に効くので、単価が時期で変わるモデルに `input_per_mtok` や
`output_per_mtok` を書くと、その変化は消えます — `models` と `doctor` がそれを
知らせます。キーはモデル自身の ID にしてください: `gemini-3.7-flash-001` のような
スナップショット別名は別エントリになり、組み込みモデルではなく標準の倍率から始まり、
その単価変更にも追従しません。全フィールドは [config.example.toml](config.example.toml) を参照。

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
| ファイル | `**/*.jsonl`（1 セッション 1 ファイル。resume は同じファイルに追記、`/clear` は新しいファイルを開く — gem-agent ADR-0071、v0.66.0） |
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
`models --json` は `models`（モデル → 現在有効な単価）と `schedule`（モデル → 全
期間。各期間の `from` は開始の瞬間の RFC 3339、最初の期間は `""`）を持ちます。
時刻は秒精度の RFC 3339 です。各行に `first_record` / `last_record`（その集計単位で
最初・最後のモデル呼び出し時刻、`--tz` の地域。`--dense` の穴埋め行など時刻を持つ
レコードが無い行は `""`）が付きます。セッション行ではそれが実行時刻です — gem-agent
v0.66.0 以降のセッション ID は UUID で、開始時刻を含みません。`--sort time` は
`first_record` 順で、`--dense` とは併用できません（時系列はキー順で既に時系列です）。

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
