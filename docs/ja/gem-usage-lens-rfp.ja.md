# RFP: gem-usage-lens

> Generated: 2026-09-02
> Status: Approved (CLI + GUI を同時開発)

## 1. Problem Statement

[gem-agent](https://github.com/nlink-jp/gem-agent)（Vertex AI Gemini バックエンドの
エージェントランタイム）を本番で日常利用しており、セッションの回転率が上がってきた。
Vertex AI の API はトークン数だけを返し金額を返さない。Cloud Billing は SKU × 日の
粒度でしかコストを出さず、セッション・ターン・呼び出しに帰属できない。
つまり「今日いくら使ったか」「今月いくらになりそうか」を知る手段が無い。

一方、gem-agent は ADR-0057（2026-08-30）以降、**すべてのモデル呼び出しについて
会計レコード（`usage`）をセッション transcript に残す**。レコードは
`source`（main / risk / compact / web_search …）・`model`・`prompt` / `output` /
`thoughts` / `cached` / `total` を持ち、ヘッダは課金リージョン（`location`）を持つ。
その ADR は「単価表とコストレポートは意図的にスコープ外 — 後で別ツールが計算できる
*可能性* を買う」と明記している。`gem-usage-lens` はその席を埋めるツールである。

`gem-usage-lens` は gem-agent のローカル transcript を解析し、トークン使用量と
コスト（Vertex AI 定価換算）を収集・計算し、SQLite に蓄積して日次 / セッション /
プロジェクト / モデル / 呼び出し種別で集計する。**予算枠は暦月**で管理する
（Claude のような週次窓型クォータは Vertex に無く、請求は月次であるため）。
GUI（`gem-usage-lens-gui`、macOS メニューバー常駐）を同時に出荷する。
ターゲットユーザーは、自分の gem-agent 利用コストをニアリアルタイムで把握し、
月次予算に対する消費ペースを知りたい個人開発者。

[claude-usage-lens](https://github.com/nlink-jp/claude-usage-lens) /
[claude-usage-lens-gui](https://github.com/nlink-jp/claude-usage-lens-gui) の対であり、
両者の骨格（SQLite ストア・増分 ingest・`--json` 契約・watch/daemon・reprice・
未価格モデルの可視化）を fork して作る。

## 2. Functional Specification

### Commands / API Surface

```
gem-usage-lens ingest            # 増分取込(idempotent)。新規/追記分のみ読み取り store へ upsert
gem-usage-lens reprice           # 単価表変更後に蓄積済み行のコストを再計算（transcript 再読不要）
gem-usage-lens report            # store を集計して表示
    --since <date|Nd|today|month>  #   期間フィルタ（month = 当月 1 日 0:00 ローカル）
    --until <date>
    --group-by hour|day|week|month|session|project|model|source  # 複数指定可（既定 day）
    --source main|risk|compact|web_search|...  # 呼び出し種別フィルタ
    --model / --project            #   部分一致フィルタ
    --sort key|cost|prompt|output|thoughts|cached|records / --top N
    --dense                        #   時系列の歯抜けをゼロ行で埋める（GUI の連続チャート用）
    --summary                      #   期間サマリ（稼働日数・日平均・ピーク日・30日換算・unpriced）
    --compare                      #   前同期間比
    --tz local|utc|<IANA>
    --json
gem-usage-lens budget            # 暦月予算の状態（消費・残量・しきい値状態・ペース予測）
    --limit-usd X --limit-tokens N   #   予算（config [budget] を上書き）
    --warn 80 --critical 95          #   しきい値 %
    --tz / --json                    #   --json が GUI との契約
gem-usage-lens sessions          # セッション一覧（id/project/model/tokens/cost/時刻）
gem-usage-lens models            # 単価表（検証日つき）と config 上書き
gem-usage-lens verify            # transcript の会計チェックサム検証（prompt+output+thoughts == total）と partial ファイルの報告
gem-usage-lens doctor            # 解決した sessions root / store / config パスと存在有無を診断
gem-usage-lens watch             # ポーリングで継続 ingest（ライブ差分表示）
gem-usage-lens daemon install|uninstall|status   # launchd で定期 ingest
gem-usage-lens version / --version
```

### Input

**入力（ローカルファイル読み取りのみ、書き込みなし）**

| 項目 | 値 |
|------|-----|
| sessions root | `$GEMAGENT_STATE_DIR/sessions` があればそれ、無ければ `~/.local/state/gem-agent/sessions`（gem-agent ADR-0022 と同じ解決順） |
| transcript | `<root>/**/*.jsonl`（現行は `projects/<エスケープ済パス>/<YYYYMMDD-HHMMSS>.jsonl`） |
| セッション ID | ファイルのベース名（gem-agent の `--resume <id>` と同じ） |

transcript の使用レコード（gem-agent ADR-0057）:

```
{"ts":"2026-09-01T00:31:43.9+09:00","kind":"session","data":{"schema":2,"version":"v0.58.0","model":"gemini-3.7-flash","project":"/path","location":"global"}}
{"ts":"...","kind":"usage","data":{"source":"main","model":"gemini-3.7-flash","prompt":57068,"output":49,"thoughts":57,"cached":0,"total":57174}}
```

会計則（ADR-0057 で実測確定）:

- `thoughts` は `output` と**別バケット**で、**output 単価で課金**される。
- `cached ⊆ prompt`: cached は prompt のうち割引になる**内数**であり加算ではない。
- `prompt + output + thoughts == total` が API 自身の数値によるチェックサム。

旧形式（ADR-0057 以前、2026-08-30 より前の gem-agent）:

- `usage` に `source` / `model` が無い → `source=main`、`model` はヘッダの値で補う。
  risk / compaction の消費は記録されていないので、そのファイルは **partial** と明示する。
- 旧 side-call レコード（`summary_usage` / `web_search` / `web_fetch` /
  `agentic_search_usage`）は `prompt` / `output` のみ持つ。`model` を持つもの
  （`summary_usage`）は thoughts=0 / cached=0 の下限値として取り込み、model が無いものは
  取り込まず partial の理由に数える。

### Dedup / 増分取込

レコードにメッセージ ID は無い。transcript は追記専用の JSONL で、resume は同一ファイルへ
追記するので、**dedup キー = `<session_id>@<行の先頭バイトオフセット>`** とし、
`ingest_state(path, last_offset)` で追記分だけを読む。ヘッダ（先頭行）は増分読み込みでも
毎回読み直す（安価）。

### Output

既定は整形テーブル（stdout）、`--json` で構造化 JSON。`report --json` の行:

```json
{"key":"2026-09-01","records":12,"prompt_tokens":1,"output_tokens":1,"thoughts_tokens":1,
 "cached_tokens":1,"total_tokens":3,"cost_usd":0.01,"partial_records":0}
```

`report --summary --json` は `unpriced_records` / `unpriced_models` を含む
（トークンがあるのに cost=0 の行 — 単価表に無いモデルの痕跡。GUI のバッジ契約）。

`budget --json`（GUI 契約）:

```json
{"window_start":"2026-09-01T00:00:00+09:00","window_end":"2026-10-01T00:00:00+09:00",
 "next_reset":"2026-10-01T00:00:00+09:00","elapsed_fraction":0.05,
 "cost":{"limit":100,"used":12.3,"remaining":87.7,"percent":12.3,"state":"normal",
         "forecast":{"projected":246,"percent":246,"exhaustion_at":"...","reliable":true,"state":"critical"}},
 "tokens":{"limit":0,"used":123456,"remaining":0,"percent":0,"state":"unset","forecast":null},
 "partial_records":0}
```

### Configuration

- config: `config.toml`。探索順は `$XDG_CONFIG_HOME/gem-usage-lens` →
  `~/.config/gem-usage-lens` → `~/Library/Application Support/gem-usage-lens`
  （最初に存在したものを読む。`doctor` が実際に読んだパスを表示する）。
  - `[sources] sessions_root` — 推定パスの上書き（`--sessions-root` フラグでも可）
  - `[pricing.models."<id>"]` — 単価の上書き/追加（`input_per_mtok` / `output_per_mtok` /
    `cache_read_multiplier` / `grounding_per_req` / `non_global_multiplier`）
  - `[budget] monthly_usd / monthly_tokens / warn_percent / critical_percent`
- store: `~/Library/Application Support/gem-usage-lens/usage.db`（SQLite, WAL, 0700/0600）
- 精度: 厳密 decode（未知キーはエラー）。部分上書きは継承（`*float64`）。

### External Dependencies

- 外部 API / サービス: **なし**（完全ローカル、ファイル読み取りのみ）
- ライブラリ: `modernc.org/sqlite`（pure-Go）、`BurntSushi/toml`
- 認証情報: 不要

## 3. Design Decisions

- **新規リポジトリとして fork し、claude-usage-lens に `--source gem` を足さない。**
  ソース形式・会計則（thoughts=output 課金、cached は内数）・予算モデル（暦月 vs
  週次校正）・store スキーマがどれも異なり、既存 GUI の JSON 契約を揺らす。名前も
  製品を指す。共通ライブラリ化もしない（2 本のスキーマが違い、共有部分は薄い）。
- **Go / util-series / 単一バイナリ**。`core/` は純粋関数 + DI で GUI が `--json` 経由で
  使う（GUI は Go を import しない）。
- **ストア = SQLite（modernc.org/sqlite, pure-Go）**。claude-usage-lens と同じ理由。
  **source transcript が消えても行を残す**（gem-agent 側の削除事故に対する保険）。
- **単価表は同梱既定 + config 上書き + `reprice`**。ADR-0057 の教訓「単価表をツールに
  焼き込まない」は承知の上で、オフライン・無資格情報で動くことを優先し、
  claude-usage-lens と同じ「検証日（`VerifiedOn`）を表に持ち、`models` が表示する」方式を
  採る。Cloud Billing Catalog API からの同期（`pricing sync`）は将来案とし v0.1 では
  作らない。表のコメントに**未来の予定価格を書かない**（検証日と出典のみ）。
- **単価モデル（Vertex AI, 2026-09-02 公式料金ページで確認）**:
  - `input_per_mtok` × prompt（cached 分を除く）+ `input_per_mtok × cache_read_multiplier`
    × cached + `output_per_mtok` × (output + thoughts)
  - `cache_read_multiplier` は per-model フィールド（既定 0.1）。定数にしない
    （claude-usage-lens の Fable 5.1 事故の再発防止）。
  - **リージョン**: ヘッダ `location` が `global` 以外なら `non_global_multiplier`
    （既定 1.1 = 公式表の「非グローバル」列）を全体に掛ける。
  - **グラウンディング**: `source=web_search` のレコード 1 件につき `grounding_per_req`
    （既定 $0.035 = 1,000 件 $35）を加算。URL context（`web_fetch`）は追加課金なし
    （トークンのみ）。
  - Flash 系に長文プレミアム無し（≤200k / >200k が同額）。3.1 Pro のような段階価格は
    v0.1 では扱わない（gem-agent の主要モデルではない。段階価格モデルは未価格として
    警告される）。
  - キャッシュの**保存料**（$/M tok·時間）はログから算出不能（暗黙キャッシュに保存料は無い）
    → 対象外と明記。
  - バッチ / Flex / 優先度ティアは gem-agent が使わないので対象外。
- **未価格モデルの可視化は初日から**: `ingest` / `reprice` の stderr 警告 +
  `report --summary` の `unpriced_records` / `unpriced_models`（保存行から導出）。
  GUI はバッジ + Reprice ボタン。
- **予算 = 暦月・ローカル TZ・毎月 1 日 0:00 リセット**。USD とトークン（`total`）の
  両基準、警告 / 危険の 2 段しきい値、経過割合からの線形ペース予測（窓の先頭 5% は
  `reliable=false`）。**校正機能は無い**（Vertex に校正対象となる窓型クォータが無い）。
  **予算の算術は CLI `core/budget` に一元化**し、GUI は `budget --json` を描画するだけ
  （claude-usage-lens-gui の `WeeklyLimit.swift` を Swift に移植しない）。
  実請求額の監視が必要なら GCP Cloud Billing の予算アラートを併用する。
- **GUI = claude-usage-lens-gui の fork（SwiftUI `MenuBarExtra`, LSUIElement, macOS 14+）**。
  同梱 CLI が信頼の基点。単一インスタンスガード、App Nap 抑止、版数表示、
  未価格バッジ、月次バジェット（popover の色付きバー + 通知）、分析ウィンドウ
  （日次 / モデル別積み上げ / 上位プロジェクト）。校正 UI は無い。
- **明示的なスコープ外**: 実請求額の照合（Cloud Billing export）、Cloud Logging の
  telemetry（`model.usage`）からの集計、マルチマシン集計（`host` 列は持たせる）、
  gem-agent 以外の Gemini クライアント、画像生成 / 文字起こしなどローカルツールの
  `image` レコード（課金対象外）。

### パッケージ構成

```
gem-usage-lens/
  main.go                 → cmd.Execute
  cmd/                    stdlib flag dispatch: ingest/reprice/report/budget/sessions/models/verify/doctor/watch/daemon
  core/model              UsageRecord{Key, Timestamp, Host, SessionID, Project, Model, Source, Location, Partial, Usage{Prompt,Output,Thoughts,Cached,Total}}, Cost
  core/collect            Discover / ReadHeader / ParseFrom（オフセット継続、旧形式の補完、チェックサム）
  core/pricing            Rates{InputPerMTok, OutputPerMTok, CacheReadMultiplier, GroundingPerReq, NonGlobalMultiplier}, Default(), Lookup（-preview/日付サフィックス正規化）, VerifiedOn
  core/cost               Compute（純関数）
  core/aggregate          group-by / dense / sort / summary(+unpriced)
  core/budget             MonthWindow / Consumption / Status / Forecast（純関数）
  core/store              SQLite: usage_records(record_key PK) / ingest_state
  core/ingest             collect → price → store
  core/config             config.toml（strict、部分上書き継承、[budget]）
  core/platform           sessions root / config 探索 / data dir / launchd
  docs/{ja,en}            RFP（本書）
```

### 永続ストアのスキーマ

```
usage_records(record_key PK, ts, host, session_id, project, model, source, location,
              prompt_tokens, output_tokens, thoughts_tokens, cached_tokens, total_tokens,
              checksum_ok, partial, cost_usd, ingested_at)
ingest_state(path PK, size, mtime, last_offset, updated_at)
```

## 4. Development Plan

### Phase 1: CLI core（本 RFP と同時に着手）
- `core/` 全パッケージ + `ingest` / `reprice` / `report` / `budget` / `sessions` /
  `models` / `verify` / `doctor` / `watch` / `daemon`
- テスト: 合成 transcript（新旧形式・resume 追記・チェックサム不一致）で parse / cost /
  aggregate / budget / store / config を固定。単価テストは「Flash の cache 0.1×」
  「非 global 1.1×」「web_search の件数課金」「thoughts が output 単価」を個別に固定
- 実データ E2E: 手元 79 transcript（うち 66 が旧形式）で ingest → report → verify

### Phase 2: GUI（CLI の JSON 契約確定後、同一リリース）
- claude-usage-lens-gui を fork し、`Row` / `Summary` / `BudgetPayload` を新契約へ
- 月次バジェット UI（設定 → CLI `budget` フラグ）、校正 UI 削除
- オフスクリーン描画（NSHostingView）で popover / 設定 / 分析の各分岐を PNG 確認

### Phase 3: Release
- 設計確定時とリリース前に独立検証パス（サブエージェント）
- CLI: `make package`（darwin/arm64 署名 + notarize）→ GitHub release → tap formula
- GUI: CLI を `VERSION=vX.Y.Z` で固定ビルドして同梱 → `make package`（notarize + staple）→
  release → tap cask
- util-series submodule 追加、org profile / web カタログ / GitHub About / tap README、
  check-org.sh

## 5. Required API Scopes / Permissions

**None.** 完全ローカルのファイル読み取りのみ。読み取り対象は利用者自身の
`~/.local/state/gem-agent/`。macOS の特別権限も不要。

## 6. Series Placement

Series: **util-series**（CLI, GUI とも）。ローカル JSONL を入力に集計結果を
テーブル / `--json` で出す pipe-friendly なデータ処理 CLI であり、claude-usage-lens /
claude-usage-lens-gui と同じ棚に置く。

## 7. External Platform Constraints

- **スキーマ drift（gem-agent 側）**: transcript 形式は gem-agent の ADR-0005 / 0057 で
  定義されるが将来変わりうる。パーサは未知フィールドを寛容にスキップし、必須フィールド
  欠落は数えて `verify` / `doctor` で見せる。`usage` レコードのフィールドが増えても
  読める。gem-agent 側でフィールドを変える ADR が出たら本ツールも追随する義務がある。
- **価格 drift（Vertex）**: gemini-3.7-flash / 3.6-flash は 2026-12-31 までの導入価格で、
  公式表は 2027-01-01 以降の価格も併記している。表には**検証日と出典のみ**書き、
  価格改定は次回の同期で反映する（`models` が検証日を表示する）。config で上書き可。
- **リージョン単価**: 公式表は global と非 global（1.1 倍）の 2 列。手元の全セッションは
  `global`。非 global の倍率は config で上書き可。
- **グラウンディング単価の粒度**: 課金は「グラウンディング プロンプト」単位で、
  gem-agent の `web_search` 1 呼び出し = 1 プロンプトと見なす。Gemini Developer API
  の「月 5,000 件無料」は Vertex には無い（Vertex は $35/1,000）。
- **旧形式の不完全さ**: 2026-08-30 以前の transcript は risk / compaction の消費を
  持たないため過小。`partial` 列で明示し、summary に partial 件数を出す。

---

## Discussion Log

- **発端（2026-09-02）**: 利用者「claude-usage-lens の gem-agent 版があるとよい。本番利用で
  回転率が上がってきた。予算枠は Claude のような週次でなく月次でよい」。
- **前提確認**: gem-agent ADR-0057 が全モデル呼び出しに `usage` レコードを書き、単価表と
  コストレポートを別ツールに委ねていることを確認。手元 79 transcript 中 66 が旧形式。
  レコードにメッセージ ID が無いことからファイル + オフセットの dedup キーを採用。
- **単価の実測**: Vertex AI 公式料金ページ（2026-09-02）で gemini-3.7-flash（global
  $0.75 / $3.75 / cached $0.075、非 global 1.1 倍、2027-01-01 から倍額）、
  gemini-3.5-flash-lite（$0.30 / $2.50 / $0.03）、gemini-3.5-flash（$1.50 / $9.00 /
  $0.15）、Google 検索グラウンディング $35/1,000 件を確認。「回答と推論」が同一の
  出力単価であることも確認。
- **確定事項**: 名称 `gem-usage-lens` / `gem-usage-lens-gui`、util-series、
  claude-usage-lens 系の fork、月次（暦月）予算、校正なし、予算算術は CLI に一元化、
  CLI と GUI を同時出荷（利用者指示）。
