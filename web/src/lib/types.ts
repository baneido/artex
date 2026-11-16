// ARTEX domain model — types used across the UI.
// Derived from the functional spec (section 7: 主要なデータ形状).

export type TaskStatus = "created" | "queued" | "running" | "paused" | "done" | "failed" | "timeout";
export type EngineMode = "exploring" | "paused" | "stalled" | "idle";

export interface Task {
  id: string;
  name?: string; // 任意のタスク名。空または未指定は未命名で、表示時は説明に戻す
  category_id?: number;
  category_name?: string;
  pinned?: boolean;
  pinned_at?: string | null;
  description: string;
  goal: string;
  status: TaskStatus;
  created_at: string;
  created_unix?: number; // created_at as unix seconds (run-duration calc)
  completed_at?: string; // RFC3339 finish time (done/failed); "" if unfinished
  completed_unix?: number; // completed_at as unix seconds (0/undef if unfinished)
  last_activity_unix?: number; // unix seconds of the last activity (0/undef if none)
  paused?: boolean;
  queued?: boolean;
  active?: boolean;
  in_flight?: number;
  findings?: { critical: number; high: number; medium: number; low: number }; // 登録済み脆弱性数（深刻度別）
  last_activity?: string;
  stalled?: boolean;
  goals_total?: number;
  goals_met?: number;
  engine_mode?: EngineMode;
  tokens?: TokenTotal; // whole-task token consumption
  llm_profile_id?: number; // LLM profile used; absent = default profile
  llm_profile_ids?: number[]; // ordered task-level failover chain
  active_llm_profile_id?: number; // profile used by the next LLM call
  llm_failover_state?: "default" | "ready" | "chain_exhausted" | string;
  llm_failover_reason?: string;
  source_task_ids?: string[]; // directly related tasks inherited as read-only context
  archive_blocked_by_task_id?: string; // live direct dependent that must be archived first
  company_ids?: number[]; // associated company scopes; current company assets join the task at creation
  coverage_enabled?: boolean; // 資産カバレッジ機能のスイッチ（作成時に決まり、既定はオン）。false はカバレッジを計算も表示もしない
}

export interface TaskCategory {
  id: number;
  name: string;
  task_count: number;
  created_at: string;
  updated_at: string;
}

export interface TaskTemplate {
  id: number;
  name: string;
  description: string;
  goal: string;
  category_id?: number | null; // 事前設定の分類。null または未指定はなし
  intercept_rules?: AssetInterceptRuleInput[]; // 事前設定のタスク単位インターセプト/許可ルール
  created_at: string;
  updated_at: string;
}

export interface DeleteTaskOptions {
  delete_assets: boolean;
  delete_traffic: boolean;
  delete_files: boolean;
  delete_findings: boolean;
  delete_llm_records: boolean;
}

export interface DeleteTaskResult {
  deleted: string;
  assets_deleted: number;
  assets_detached: number;
  traffic_deleted: number;
  files_deleted: boolean;
  findings_deleted: number;
  llm_records_deleted: number;
  cleanup_warning?: string;
}

export type TaskArchiveState =
  | "archive_queued"
  | "archiving"
  | "archive_failed"
  | "ready"
  | "restore_queued"
  | "restoring"
  | "restore_failed"
  | "delete_queued"
  | "deleting"
  | "delete_failed";

export interface TaskArchiveTokenStats {
  calls?: number;
  input_tokens?: number;
  output_tokens?: number;
  cache_read_tokens?: number;
  cache_write_tokens?: number;
}

export interface TaskArchive {
  id: number;
  task_id: number;
  state: TaskArchiveState;
  phase: string;
  progress: number;
  error?: string;
  warnings?: string[];
  format_version: number;
  sha256?: string;
  original_size: number;
  compressed_size: number;
  task_name: string;
  task_description: string;
  task_goal: string;
  original_status: TaskStatus;
  category_id?: number;
  category_name?: string;
  source_task_ids: number[];
  remaining_timeout_seconds: number;
  data_counts: Record<string, number>;
  aggregate_stats: {
    tokens?: TaskArchiveTokenStats;
    skills?: Record<string, number>;
    tools?: Record<string, number>;
    findings?: Record<string, number>;
  };
  archived_at?: string;
  requested_at: string;
  created_at: string;
  updated_at: string;
}

export interface TaskArchivePage {
  items: TaskArchive[];
  total: number;
  page: number;
  size: number;
}

export interface ArchiveBatchItem {
  id: string;
  archive_id?: number;
  ok: boolean;
  queued: boolean;
  error?: string;
}

// ---- Asset graph (global, shared across tasks) ----
export type AssetType =
  | "company"
  | "domain"
  | "ip"
  | "port"
  | "service"
  | "site"
  | "endpoint"
  | "parameter"
  | "tech"
  | "credential"
  | "data";

export type NodeState = "observed" | "confirmed" | "tombstoned";

export interface AssetNode {
  id: string;
  type: AssetType;
  name: string;
  key: string; // nkey
  value?: string;
  company_id?: string; // 所属企業の資産 id。空は未所属
  state: NodeState;
  confidence: number; // 0..1
  attrs?: Record<string, unknown>;
  first_seen: string;
  last_seen: string;
}

export type AssetRel =
  | "owns"
  | "resolves"
  | "exposes"
  | "runs"
  | "serves"
  | "has_endpoint"
  | "has_param"
  | "fingerprinted"
  | "authenticates_as"
  | "reachable"
  | "has_subdomain";

export interface Edge {
  src: string;
  dst: string;
  rel: AssetRel | ExploreRel;
}

// Task asset view — server-side enriched, paginated.
export interface TaskAssetRef {
  id: string;
  name?: string;
  key: string;
  attrs?: Record<string, unknown>;
}

export interface TaskAssetItem extends AssetNode {
  techs?: TaskAssetRef[];
  auth?: TaskAssetRef[];
  params?: TaskAssetRef[];
}

export interface TaskAssetView {
  counts: Record<string, number>;
  total: number;
  items: TaskAssetItem[];
}

// ---- New unified asset model (new backend) ----
export type NewAssetType = "root_domain" | "ip" | "subdomain" | "app" | "service" | "endpoint";

export interface Asset {
  id: number;
  type: NewAssetType;
  company_id?: number;
  task_ids: number[];
  domain?: string;
  root_domain?: string;
  ip?: string;
  c_segment?: string;
  port?: number;
  icp?: string;
  bound_domains?: string[];
  open_ports?: { port: number; service?: string }[];
  record_type?: string;
  record_value?: string[] | string;
  bundle_id?: string;
  app_name?: string;
  category?: string;
  app_description?: string;
  app_icp?: string;
  url?: string;
  service_type?: string;
  service_name?: string;
  favicon_mmh3?: string;
  status_code?: number;
  content_length?: number;
  page_title?: string;
  technologies?: string[];
  auth?: Record<string, unknown>[];
  method?: string;
  params?: Record<string, unknown>[];
  extra?: Record<string, unknown>;
  last_seen: string;
  task_source?: string;
  task_source_summary?: string;
  task_source_node_id?: number;
}

export interface IntentAsset {
  intent_id: number | string;
  asset_id: number;
  type: NewAssetType;
  label: string;
  source: string;
  source_summary: string;
  source_node_id?: number;
  source_task_id: number;
  inherited: boolean;
}

export interface TaskAssetMutation {
  requested: number;
  attached: number;
  existing: number;
}

export interface TaskAssetScopeMutation {
  requested: number;
  assets_linked: number;
  assets_existing: number;
  scopes_added: number;
  scopes_existing: number;
}

// ---- Asset coverage graph (per task) ----
// 力指向の「資産カバレッジ図」の1ノード。key は一意。資産は "a:<id>"、企業は "c:<id>"、
// 資産行のないルートドメインは "r:<domain>"。in_scope=false は結線のためだけの灰色の文脈ノード。
export interface CoverageGraphNode {
  key: string;
  kind: "company" | "root_domain" | "subdomain" | "ip" | "service" | "app" | "endpoint";
  label: string;
  tested: boolean;
  in_scope: boolean;
  asset_id?: number;
  company_id?: number;
  domain?: string;
  root_domain?: string;
  ip?: string;
  url?: string;
  port?: number;
  service_type?: string;
  app_name?: string;
  page_title?: string;
  status_code?: number;
}

export interface CoverageGraphEdge {
  src: string;
  dst: string;
}

export interface CoverageGraphData {
  nodes: CoverageGraphNode[];
  edges: CoverageGraphEdge[];
}

// ある資産がこのタスクの探索グラフで関連付くインテント、事実、検出事項（カバレッジ図ノードのドロワー用）。
export interface CoverageAssetRef {
  id: number;
  kind: string;
  state: string;
  summary: string;
  source_task_id?: string;
  inherited?: boolean;
}
export interface CoverageAssetRefs {
  intents: CoverageAssetRef[];
  facts: CoverageAssetRef[];
  findings: CoverageAssetRef[];
}

// ---- Workspace file manager (workDir) ----
export interface WorkspaceEntry {
  name: string;
  path: string; // workspace-relative, forward slashes
  dir: boolean;
  size: number;
  mtime: number; // unix millis
}
export interface WorkspaceListing {
  path: string;
  entries: WorkspaceEntry[];
}
export interface WorkspaceFile {
  path: string;
  size: number;
  binary: boolean;
  too_large?: boolean;
  content?: string;
}

// タスクのテスト範囲の1件（カバレッジの分母と認可境界）。
export interface TaskScopeRow {
  id: number;
  task_id: number;
  kind: "company" | "root_domain" | "subdomain" | "ip" | "cidr" | "icp" | "keyword";
  company_id?: number;
  company_name?: string; // バックエンドが companies を JOIN して解決する。kind=company のときだけ値がある
  domain?: string;
  net?: string;
  value?: string;
  source: "auto" | "agent" | "manual";
  reason?: string;
}

export type CompanyScopeKind = "domain" | "ip" | "cidr" | "icp" | "keyword";

// 企業を追加するときに送る、構造化された資産範囲ルール。
export interface CompanyScopeRule {
  kind: CompanyScopeKind;
  value: string;
}

// 資産範囲を書き込んだ結果。errors は今回の送信で不正な行。warnings は今回の送信とは無関係だが、
// 所属結果が想定とずれる既存データの問題（ip フィールドにホスト名が入った資産など）。
export interface CompanyScopeMutation {
  added: number;
  skipped: number;
  invalid: number;
  errors?: string[];
  warnings?: string[];
}

// 企業の資産範囲ルールの1件（所属の唯一の正）。
export interface ScopeRow {
  id: number;
  company_id: number;
  kind: CompanyScopeKind;
  domain?: string; // kind=domain のとき値がある
  net?: string; // kind=ip|cidr のとき値がある
  value?: string; // kind=icp|keyword のときバックエンドが直接返すことがある
  raw: string; // ユーザーの元の入力。表示と再入力に使う
  reason?: string;
}

// 企業。type=company の資産ノード、アイコン、資産数、資産範囲ルール。
export interface Company {
  id: number;
  name: string;
  logo?: string; // リモートのアイコン URL。空ならフロントは名称の頭文字を使う
  asset_count: number;
  scope?: ScopeRow[];
}

// ---- Exploration graph (per task) ----
export type ExploreKind = "task" | "begin" | "goal" | "intent" | "fact" | "finding" | "hint" | "digest";
export type GoalState = "open" | "met" | "abandoned";
export type IntentState = "open" | "running" | "paused" | "done" | "blocked" | "exhausted" | "stopped";
export type FindingState = "confirmed" | "dismissed";
export type HintState = "active" | "consumed";
export type ExploreRel = "spawns" | "derived_from" | "yields" | "proves" | "covers";

export interface TaskNode {
  id: string;
  type: ExploreKind;
  payload?: string;
  priority: number; // 0..10
  state: string; // GoalState | IntentState | FindingState | HintState
  origin: string;
  ts: string;
  source_task_id?: string;
  inherited?: boolean;
  delete_reason?: string; // インテントの論理削除（state='deleted'）のときの削除理由
}

// ブロードキャストボードの1ページ。作成順でページ分割したノード、このページに関わる辺、辺の反対側のノード（refs、id で引く）。
// これで各投稿が「どこから来て、何を出したか」を説明でき、グラフ全体を取らなくてよい。
export interface ExplorationNodePage {
  items: TaskNode[];
  total: number;
  page: number;
  size: number;
  edges: Edge[];
  refs: Record<string, TaskNode>;
  // ノード id から、そのノードがアンカーする資産（ブロードキャストボードを展開するときに併せて出す。このページのノードとその隣接を含む）。
  assets: Record<string, FindingAsset[]>;
}

export interface ExplorationNodeQuery {
  page?: number;
  size?: number;
  kinds?: ExploreKind[];
  states?: string[];
  q?: string;
  order?: "asc" | "desc";
}

// 目標管理カード用の目標（バックエンドが payload を text/vulnclass に分け済み）。
export interface TaskGoal {
  id: string;
  text: string;
  vulnclass?: string;
  state: string; // GoalState
  origin?: string;
  ts: string;
}

// 制約管理カード用の操作制約（allow=許可 / deny=禁止）。
export type ConstraintKind = "allow" | "deny";
export interface TaskConstraint {
  id: string;
  kind: ConstraintKind;
  text: string;
  origin?: string;
  ts?: string;
}

// ---- Findings ----
export type Severity = "critical" | "high" | "medium" | "low";

// 脆弱性の対応状態。未対応 / 対応中 / 確認済み / 対応済み / 修正済み / 誤検知 / 無視 / 重複 / リスク受容。
export type FindingStatus =
  | "pending"
  | "in_progress"
  | "confirmed"
  | "resolved"
  | "fixed"
  | "false_positive"
  | "ignored"
  | "duplicate"
  | "risk_accepted";

// FindingAsset は脆弱性に関連付いた資産（label はバックエンドで事前に描画済み）。
export interface FindingAsset {
  id: string;
  type: string;
  label: string;
}

export interface Finding {
  traffic_count?: number;
  evidence_version?: number;
  report_evidence_version?: number;
  report_stale?: boolean;
  id: string;
  finding_id?: string; // 独立した findings 表の行 id。状態更新のハンドル（タスク内の古いノードには無いことがある）
  vulnclass: string;
  name?: string; // 脆弱性の名称。空のときは表示を vulnclass に戻す
  severity: Severity;
  status: FindingStatus;
  summary: string;
  evidence: string;
  report?: string; // 詳細レポート（Markdown）。詳細 API だけが返し、一覧では空
  intent_id?: string;
  param_id?: string;
  task_id?: string;
  task_description?: string;
  source_task_id?: string;
  inherited?: boolean;
  assets?: FindingAsset[];
  ts: string;
}

// FindingsPage は検出事項一覧のサーバー側ページング応答である。
export interface FindingsPage {
  items: Finding[];
  total: number;
  page: number;
  page_size: number;
}

export interface FindingGroup {
  task_id: string | number | null;
  task_name?: string; // 任意のタスク名。空または未指定は未命名
  task_description: string;
  task_status: string;
  count: number;
  critical: number;
  high: number;
  medium: number;
  low: number;
  last_found_at: string;
}

export interface FindingGroupsPage {
  items: FindingGroup[];
  total: number;
  finding_total: number;
  page: number;
  page_size: number;
}

export interface FindingDeepenResponse {
  task_id: string;
  intent_id: string;
  state: IntentState;
  queued: boolean;
}

// FindingStats は検出事項の全表集計（統計カードと脆弱性種別のプルダウン）。サーバーで計算し、ページングの影響を受けない。
export interface FindingStats {
  total: number;
  pending: number;
  critical: number;
  high: number;
  medium: number;
  low: number;
  vulnclasses: string[];
  tasks: FindingTaskOption[];
}

// FindingTaskOption は検出事項ページの「タスク別」絞り込みの1項目。脆弱性があるタスク（説明が空ならタスクは削除済みで、
// フロントは id を表示する）とその脆弱性件数。
export interface FindingTaskOption {
  id: string | number;
  name?: string; // 任意のタスク名。空または未指定は未命名
  description: string;
  count: number;
}

// FindingQuery は検出事項一覧のページング、絞り込み、並び替えのパラメータである。
export interface FindingQuery {
  page: number;
  pageSize: number;
  severity?: "all" | Severity;
  status?: "all" | FindingStatus;
  vulnclass?: string;
  task?: string; // タスク id。"all" または空はタスクで絞らない
  query?: string;
  sort?: "severity" | "time";
  // 資産ツリーのノード key。1ノードを選ぶとそのサブツリー全体を選ぶ。空は資産で絞らない。
  assetScope?: string;
}

// ---- Findings by asset (資産ビュー) ----
export type FindingAssetKind = "company" | "root_domain" | "subdomain" | "ip" | "service" | "app" | "endpoint" | "none";

// FindingAssetNode は資産ツリーの1ノード。key は a:<id>（資産）、c:<id>（企業）、
// r:<domain>（庫に資産行の無いルートドメイン）、__none__（未関連の資産）。
export interface FindingAssetNode {
  key: string;
  parent?: string;
  kind: FindingAssetKind;
  label: string;
  asset_id?: number;
  company_id?: number;
  self: number; // その資産に直接付いた検出事項の数
  total: number; // 子孫を含み、検出事項で重複排除
  critical: number;
  high: number;
  medium: number;
  low: number;
  last_found_at: string;
}

export interface FindingAssetTree {
  nodes: FindingAssetNode[];
  finding_total: number;
  truncated: boolean;
  dropped_kinds?: string[];
}

// FINDING_UNASSIGNED_ASSET はバックエンドの db.FindingUnassignedAsset に対応する。
export const FINDING_UNASSIGNED_ASSET = "__none__";

// ---- Activity / sessions ----
export type ActivityKind =
  | "tool_use"
  | "tool_result"
  | "text"
  | "thinking"
  | "result"
  | "user"
  | "intent" // LLM-generated exploration objective leading a worker session (UI-synthesized)
  | "round" // planner round boundary marker (engine-emitted)
  | "usage" // live cumulative token usage (per model turn); not rendered
  | "llm_switch" // automatic/manual task-level LLM switch
  | "llm_failover" // task-level provider switch / chain exhaustion audit event
  | "intercept_request"; // user-approval request from the intercept layer

// ChatAttachment は1回のアップロードファイル。path はそのセッションまたはタスクの作業ディレクトリ（agent の CWD）からの相対パス。
export interface ChatAttachment {
  name: string;
  path: string;
  size: number;
  abs?: string; // 絶対パス（scope=staging の一時アップロード時に返す。タスク作成前に説明へ書き込む）
}

export interface Activity {
  seq: number;
  intent_id?: string;
  worker: string; // session owner: planner | mainagent | work#1 ...
  ts: string;
  kind: ActivityKind;
  tool?: string;
  tool_use_id?: string;
  is_error?: boolean;
  summary: string;
  detail?: string;
  metadata?: {
    llm_transition?: LLMTransition;
  };
  source_task_id?: string;
  inherited?: boolean;
  main_seg?: number; // main-agent conversation segment (present only on worker="mainagent" rows)
  // token usage (present only on kind='result')
  input_tokens?: number;
  output_tokens?: number;
  cache_read_tokens?: number;
  cache_write_tokens?: number;
}

export interface LLMAuditProfile {
  id: number;
  name: string;
  format: string;
  model: string;
}

export interface LLMTransition {
  mode: "automatic" | "manual" | "exhausted";
  reason: string;
  previous?: LLMAuditProfile;
  next?: LLMAuditProfile;
}

export interface TaskLLMResolution {
  profile_id?: number;
  name: string;
  format: string;
  model: string;
  source: "task_chain" | "agent_binding" | "global_profile" | "environment" | "global";
  available: boolean;
  reason?: string;
}

export interface TaskLLMResolutions {
  mainagent: TaskLLMResolution;
  planner: TaskLLMResolution;
  worker: TaskLLMResolution;
}

// ---- Agent triggers（P3 のスケジューラ。カスタム agent だけ） ----
export interface AgentTrigger {
  id: number;
  agent_key: string;
  enabled: boolean;
  interval_sec: number; // 定期。N 秒ごと（0 は定期ではない）
  on_finding: boolean; // 任意のタスクが finding を検出したときに発火
  on_goal_met: boolean; // 任意のタスクが目標を達成したときに発火
  on_task_timeout: boolean; // 任意のタスクがタイムアウトしたときに発火
  on_tool_call: boolean; // 選択したツールが呼ばれた（実行完了）ときに発火
  on_task_create: boolean; // 任意のタスクが作成されたときに発火
  interval_message: string; // 各発火条件ごとの独立したユーザーメッセージ
  finding_message: string;
  goal_message: string;
  task_timeout_message: string;
  tool_call_message: string;
  task_create_message: string;
  tool_names: string[]; // on_tool_call で選んだツール key（1つ以上）
  last_fire?: string;
}

// ---- Conversations (chat page) ----
export interface ActiveFindingRetest {
  id: number;
  finding_id: string;
  conversation_id: number;
  status: "pending" | "running";
}

export interface FindingRetest {
  id: number;
  finding_id: number;
  conversation_id: number | null;
  status: "pending" | "running" | "completed" | "failed" | "stopped";
  verdict: "" | "reproduced" | "fixed" | "inconclusive";
  notes: string;
  summary: string;
  evidence: string;
  error: string;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
}

export interface Conversation {
  id: number;
  running?: boolean; // live server state, returned with the conversation list
  agent_key: string;
  title: string;
  llm_profile_id?: number;
  pinned?: boolean;
  pinned_at?: string | null;
  created_at: string;
  updated_at: string;
}

// ---- Backend logs (/logs page) ----
export interface LogLine {
  seq: number;
  db_id?: number; // server_logs.id; present for DB-persisted lines
  ts: string;
  level: "info" | "warn" | "error";
  tag: string;
  text: string;
}

export type SessionRole = "mainagent" | "planner" | "worker" | "system";
export type SessionStatus = "running" | "paused" | "done" | "blocked" | "exhausted" | "pending" | "stopped" | "deleted";

// Daily token aggregate bucket (GET /api/tokens/daily).
export interface DailyTokenBucket {
  date: string; // "YYYY-MM-DD"
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// Per-worker token usage (GET /api/exploration/tokens).
export interface TokenUsage {
  worker: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

export interface SessionTokenUsage {
  session: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

export interface BatchControlItem {
  id: string;
  ok: boolean;
  status?: string;
  queued?: boolean;
  error?: string;
}

// 分類の一括変更のタスクごとの結果。失敗はタスクがすでに削除されている場合だけ。分類そのものの書き込みは原子的。
export interface BatchCategoryItem {
  id: string;
  ok: boolean;
  error?: string;
}

// Whole-task (all agents) token aggregate.
export interface TokenTotal {
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// Global per-profile token spend from the llm_usage ledger (GET /api/tokens/usage).
export interface ProfileUsage {
  profile_name: string;
  calls: number;
  tasks: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// One (profile, UTC day) token bucket for the dashboard's daily chart (new source).
export interface ProfileDayUsage {
  profile_name: string;
  date: string; // YYYY-MM-DD
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
}

// Response of GET /api/tokens/usage — the dashboard's "new" (llm_usage) token view.
export interface UsageStats {
  by_profile: ProfileUsage[];
  daily: ProfileDayUsage[];
}

// Per-model token usage for one task (GET /api/llm/records/by-model), from the
// always-on llm_usage metering ledger. calls = number of LLM calls on this model.
export interface ModelTokenStat {
  model: string;
  calls: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

export interface Session {
  id: string;
  role: SessionRole;
  title: string;
  status: SessionStatus;
  live: boolean;
  last_activity: string;
  intent_id?: string;
  source_task_id?: string;
  inherited?: boolean;
  seg?: number; // main-agent session: which conversation segment (0 = original)
}

// ---- Security ----
export interface AuditEntry {
  ts: string;
  tool: string;
  action: "allow" | "block";
  reason?: string;
  command?: string;
}

export interface Audit {
  entries?: AuditEntry[];
  attributions?: Record<string, number>;
}

// ---- Traffic ----
export interface TrafficExchange {
  id: string;
  ts: string;
  host: string;
  method: string;
  url: string;
  status: number;
  content_type: string;
  resp_len: number;
}

export interface TrafficResp {
  enabled: boolean;
  proxy?: string;
  count?: number; // global total (unfiltered)
  total?: number; // rows matching the current filter (for pagination)
  page?: number;
  size?: number;
  exchanges?: TrafficExchange[];
}

// Full raw request/response of one exchange (lazy-loaded on row select).
export interface TrafficDetail {
  req: string;
  resp: string;
}

// One distinct recorded host with its exchange count (target picker).
export interface TrafficHost {
  host: string;
  count: number;
}

// ---- App settings (runtime toggles) ----
export interface Settings {
  traffic_capture: boolean;
  agent_traffic_binding: boolean; // Agent がトラフィック証拠を自動で関連付ける。既定はオフ。手動の関連付けには影響しない
  llm_record: boolean; // LLM 記録のスイッチ（既定はオフ）。オフのときは LLM 呼び出しを一切記録しない
  // Web search. brave_key_set / tavily_key_set reflect whether a key is stored
  // (the values are never returned). On PUT, send the corresponding field to set/clear.
  web_search_enabled: boolean;
  web_search_backend: string; // "ddgs" | "brave-free" | "tavily" | "deepseek"
  brave_key_set: boolean;
  tavily_key_set: boolean;
  // write-only: only sent on PUT to store/clear the key.
  brave_search_api_key?: string;
  tavily_search_api_key?: string;
  // 検索エンドポイントへ出る独立した出口プロキシ（http/https/socks5）。トラフィックを記録する MITM プロキシとは無関係。空は直接接続。
  web_search_proxy?: string;
  // グローバル出口プロキシ（http/https/socks5。user:pass を付けられる）。対象のトラフィックはすべてここを通る。トラフィック取得がオンのときは
  // MITM の上流になる。取得がオフのときは agent の bash/WebFetch へ直接注入する。空は直接接続。
  global_proxy?: string;
  python_interpreter?: string; // カスタムスクリプトツールの python インタプリタのパス（空は実行時に検出）
  workers?: number; // 並行する作業 agent の数（既定 3）。以後起動するタスクに効く
  // タスク同時実行の上限。同時に「実行中」になれるタスク数。オフは無制限。オンのあと、新規タスクが上限を超えると待ち、空きが出たら自動で起動する。
  task_concurrency_enabled?: boolean; // 既定 false
  task_concurrency_limit?: number; // オンにしたあとの既定は 5
  // LLM のローテーション（フェイルオーバー）。既定はオフ。オンのあと、「モデル未指定」の agent は現在の設定が使えないとき
  // （残高不足、key 失効、レート制限、サービス異常）次の設定へ自動で切り替える。
  llm_pool_enabled?: boolean; // 既定 false
  // 指定設定を関連付けた agent やタスクが失敗したときも、ローテーション鎖へ戻すか。既定 false は、関連付けたらそれを専有する。
  llm_pool_bind_fallback?: boolean;
  // 操作制約を注入する範囲（既定はすべてオン）。タスクの allow/deny 制約を、対応する agent のシステムプロンプトへつなぐ。
  constraints_inject_planner?: boolean;
  constraints_inject_worker?: boolean;
  // 実験機能。noa のモデル駆動コンテキスト圧縮（既定はオフ）。オンにすると、プラットフォームに接続する4種の agent（planner、
  // worker、メイン agent、対話）のコンテキスト圧縮を noa が引き取り、組み込み compaction を置き換える。run ごとに1回読み、
  // 以後起動する run に効く。
  noa_compaction?: boolean;
  // ---- 脆弱性の IM 通知（チャネル自体は独立したリソース。/api/notify/* を参照。ここにあるのはグローバル設定3つだけ）----
  notify_enabled?: boolean; // 通知の全体スイッチ。既定はオン。保守中に一度で止めるためのもの
  notify_public_base_url?: string; // 脆弱性詳細へ戻る外部 URL。空ならメッセージにリンクを付けない
  notify_digest_interval_min?: number; // まとめモードの周期（分）。既定 30
}

// ---- 脆弱性の IM 通知 ----

// NotificationFilter はチャネルの絞り込み条件。フィールドはすべて任意で、未指定は絞らない。
// バックエンドはどのフィールドも検証しない。設定が壊れているときは「一致」として扱う（漏れるより多く送る）。
export interface NotificationFilter {
  min_severity?: string; // "" | low | medium | high | critical
  task_ids?: number[]; // 空は制限なし。空でないときは、脆弱性の所属タスクと交わることを要求する
  asset_ids?: number[]; // 空は制限なし。空でないときは、脆弱性がアンカーする資産と交わることを要求する
  vulnclass_include?: string[]; // 空はすべて受け取る。空でないときは、脆弱性種別がいずれかのキーワードに当たることを要求する（大文字小文字を区別しない部分文字列）
  vulnclass_exclude?: string[]; // いずれかのキーワードに当たれば除外する（除外は包含より優先）
  on_status_change?: boolean; // 脆弱性の対応状態の変更イベントも受け取るか
}

// NotificationChannel はチャネルの1インスタンス。config のフィールドは kind によって違う。
// 資格情報フィールドは読み出し時に "__masked__" で始まるマスク値へ置き換わる。そのまま送り返すと「変更しない」という意味になる。
export interface NotificationChannel {
  id: number;
  name: string;
  kind: string;
  enabled: boolean;
  mode: "realtime" | "digest";
  config: Record<string, unknown>;
  filter: NotificationFilter;
  rate_per_min: number;
  created_at: string;
  updated_at: string;
  // secret_keys はバックエンドがチャネル種別ごとに返す。フロントはこれでパスワード欄と「空のままなら変更しない」を描画し、
  // チャネルの知識をハードコードしない。
  secret_keys: string[];
}

// NotificationKind は /api/notify/meta が返すチャネル種別のメタデータである。
export interface NotificationKind {
  kind: string;
  default_rate_per_min: number;
  secret_keys: string[];
}

export interface NotificationMeta {
  kinds: NotificationKind[];
  enabled: boolean;
  public_base_url: string;
  digest_interval_min: string;
  defaults: { digest_interval_min: number };
  stats: {
    channels: number;
    channels_on: number;
    pending: number;
    failed: number;
    sent_today: number;
    backlog_age_ms: number;
  };
}

// NotificationDelivery は配信記録の1件。配信履歴と失敗の再送に使う。
export interface NotificationDelivery {
  id: number;
  finding_id: string;
  event_kind: string; // finding_created | finding_status_changed
  channel_id: number;
  channel_name: string;
  channel_kind: string;
  state: "pending" | "sending" | "sent" | "failed" | "skipped";
  attempts: number;
  last_error: string;
  batch_id?: number;
  created_at: string;
  sent_at?: string;
  next_attempt_at: string;
  title: string;
  severity: string;
}

// ---- LLM config ----
export interface LLMProfile {
  id: string;
  name: string;
  format: "openai" | "anthropic" | "openai-responses";
  base_url?: string;
  proxy?: string;
  model: string;
  api_key_hint?: string;
  rate_per_second: number;
  rate_per_minute: number;
  context_window_k?: number;
  // 思考スイッチ（thinking.type）。"" は送らない（既定） | "disabled" はオフ | "enabled" はオン
  thinking_type?: string;
  // 思考の強さ。"" は送らない（既定） | "low" / "medium" / "high" / "xhigh" / "max"
  reasoning_effort?: string;
  is_default: boolean;
  // ローテーションの順位。大きいほど先に選ばれる。有効な設定は常に鎖の先頭で、この値とは無関係。
  priority?: number;
  // true はフェイルオーバー先にしない（agent やタスクが明示的に関連付けて使うことはできる）。
  pool_exclude?: boolean;
  // true（既定）はストリーミング（SSE）。false は本当の非ストリーミング（stream:false、一度に返す）。
  streaming?: boolean;
  // 1回の応答の出力上限（token）。0 はそのフィールドを送らず、サーバーの既定値に任せる。
  // context_window_k とは分ける。あちらはモデルの総容量で、ローカルの圧縮しきい値にだけ使う。
  max_tokens?: number;
  // 上限に使うリクエストフィールド名。format="openai" のときだけ意味がある。
  // "" は max_tokens（既定） | "max_completion_tokens"（OpenAI の推論モデルはこれだけを認める）
  max_tokens_field?: string;
  // カスタムのセッションヘッダ名。空でないときは毎回のリクエストにその HTTP ヘッダを付ける。値は現在のセッションまたはインテントの session id。
  // "" は送らない。session-id ヘッダでプロンプトキャッシュやスティッキーなルーティングをするゲートウェイ向け。
  session_header_key?: string;
  // この設定による再試行の上書き（接続確立、空レスポンス、同じ provider の安全窓）。空、またはすべて 0 はグローバル方針に従う。
  retry?: LLMRetryOverride;
}

// ---- LLM 再試行方針 ----
// 1層の再試行の2つのつまみ。どちらも「0 = 未設定」。
//   attempts    0 は既定回数 | -1 はその層の再試行をオフ | >0 は再試行回数
//   interval_ms 0 は既定の指数バックオフ | >0 はこの固定ミリ秒間隔に変える
export interface LLMRetryRule {
  attempts: number;
  interval_ms: number;
}

// 1つの LLM 設定が上書きできる3層（どれも「エンドポイントに付いていく」再試行）。
export interface LLMRetryOverride {
  connect: LLMRetryRule; // 接続確立の再試行。接続リセット、タイムアウト、429、5xx。ストリーム開始前
  empty: LLMRetryRule; // 空レスポンスの再試行。完了したが内容が無い（openai 形式だけ）
  stream: LLMRetryRule; // 同じ provider の安全窓での再試行。出力を渡す前の切断を再送する
}

// グローバル方針は、上の3層の既定値に、グローバルにしか無い2層を足したもの。
//   breaker ローテーションのサーキットブレーカ（attempts は連続した即時失敗が何回で遮断するか、interval_ms は固定の冷却時間）
//   intent  インテントの再実行（worker が model_error で終わったあと、そのインテント全体を再実行する）
export interface LLMRetryPolicy extends LLMRetryOverride {
  breaker: LLMRetryRule;
  intent: LLMRetryRule;
}

// ---- LLM ローテーション（フェイルオーバー）----
// ある設定の、ローテーション鎖での位置と健全性。state は次のとおり。
//   ok       正常
//   degraded 連続失敗はあるが、遮断の閾値には達していない
//   tripped  遮断済み。冷却中は飛ばす（cooldown_secs は残り秒）
export interface LLMPoolMember {
  profile_id: string;
  name: string;
  model: string;
  format: string;
  priority: number;
  active: boolean; // 現在有効な設定か（常に鎖の先頭）
  excluded: boolean; // pool_exclude。ローテーションに参加しない
  state: "ok" | "degraded" | "tripped";
  fails: number;
  trips: number;
  cooldown_secs: number;
  last_error?: string;
  last_at?: string;
}

export interface LLMPoolStatus {
  enabled: boolean;
  bind_fallback: boolean;
  chain: LLMPoolMember[];
}

// ---- Agents ----
export interface Agent {
  id: string;
  key: string; // 組み込みは goals/planner/mainagent/worker。カスタムはユーザーが決めた key
  name: string;
  description?: string;
  role: string;
  builtin: boolean;
  enabled: boolean;
  llm_profile_id?: number | null; // 関連付けた LLM 設定。null または absent はタスク、セッション、グローバルに従う
  max_turns?: number; // 0 = 無制限
  run_seconds?: number; // worker の1回の実行の壁時計上限（秒）。0 は無制限
  web_search?: boolean; // ネットワーク検索を有効にするか（システムの全体スイッチに従う）
  interactive_shell?: boolean; // 対話式 shell を有効にするか（永続 PTY セッションのツール群）
  // P3 の発火後の処理方針（カスタム agent だけに意味がある）
  trigger_run_mode?: "serial" | "parallel"; // 直列で待つ / 発火のたびに別セッションを並行する
  trigger_merge_mode?: "by_task" | "all" | "none"; // serial だけ。同じタスクをまとめる / 全部まとめる / まとめない
  trigger_max_parallel?: number; // parallel だけ。agent ごとの並行上限。0 は無制限
  // 関連付け数（一覧 API だけが返す）。見える MCP / 見える Skill / 関連付けたツール
  mcp_count?: number;
  skill_count?: number;
  tool_count?: number;
}

export interface PromptVar {
  name: string;
  description: string;
  example: string;
  source: "exploration" | "runtime" | "distilled";
}

export interface PromptVersion {
  version: number;
  ts: string;
  note: string;
  template_text: string;
}

export interface AgentDetail {
  agent: Agent;
  prompt: string;
  variables: PromptVar[];
  versions: PromptVersion[];
  visibility: { mcp: number[]; skill: string[] };
  // 関連付けできる LLM 設定の候補（「既定モデル」のプルダウン用）。現在の関連付けは agent.llm_profile_id
  llm_profiles?: { id: number; name: string; model: string; is_default: boolean }[];
  wrapup_prompt?: string; // 保存済みの締めプロンプト（空は組み込み既定）
  wrapup_default?: string; // 組み込み既定の締めプロンプト（プレースホルダと既定への復帰）
  wrapup_max_turns?: number; // 保存済みの締めターン数（0 は組み込み既定）
  wrapup_max_turns_default?: number; // 組み込み既定の締めターン数（"0=既定N" の表示用）
  // タスク単位のタイムアウト締め文言（worker と planner だけ。task_timeout_wrapup_supported=true のときだけその区画を出す）
  task_timeout_wrapup_supported?: boolean;
  task_timeout_wrapup_prompt?: string;
  task_timeout_wrapup_default?: string;
  task_timeout_wrapup_max_turns?: number;
  task_timeout_wrapup_max_turns_default?: number;
}

// ---- MCP ----
export interface MCPServer {
  id: number;
  name: string;
  transport: "stdio" | "http" | "sse";
  command?: string;
  args: string[];
  env: Record<string, string>;
  url?: string;
  enabled: boolean;
  insecure?: boolean; // http: skip TLS cert verification (self-signed servers)
  tools?: string[]; // mcp_tools_cache (names only, for the count)
}

export interface MCPTool {
  name: string;
  description: string;
}

// ---- Skills ----
// Fields align with the agentskills.io open specification.
// description covers both "what the skill does" and "when to use it".
export interface SkillItem {
  name: string; // unique key = directory name
  description?: string; // required per spec; covers what + when to use
  license?: string; // optional: SPDX identifier or free text
  compatibility?: string; // optional: environment requirements
  mcps?: string[]; // MCP server names this skill unlocks on load
  files: string[]; // files in the skill directory
  // 呼び出し統計（skill_usage 台帳）。一度も呼ばれていない skill は calls=0、last_used は未指定。
  calls: number;
  tasks: number; // それを読み込んだタスク数（chat セッションは数えない）
  usage_agents: string[]; // それを読み込んだ agent key
  last_used?: string;
}

// SkillCall は Skill() の1回の呼び出し（1つの skill の最近の呼び出し一覧）。
export interface SkillCall {
  ts: string;
  agent_key: string;
  task_id: number; // 0 はタスク以外（対話セッション）
  session_id: string;
  args_len: number;
}

// MissingSkill は名前を指定されたが存在しない skill。「使いたいが無い」不足。
export interface MissingSkill {
  skill: string;
  calls: number;
  agents: string[];
  last_used?: string;
}

// ---- Tools（組み込みツール一覧） ----
// key + handler live in Go; only these fields are page-editable. system tools lock
// the key and the parameter *structure* (name/type/required) — the per-param
// description/default and the agent binding are what move.
export interface Tool {
  key: string;
  system: boolean;
  description: string;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  schema: Record<string, any>; // full JSON-Schema (object with properties)
  agents: string[]; // bound agent keys
  enabled: boolean;
  kind?: "builtin" | "shell" | "command" | "script" | "http"; // カスタムツールの種別
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  exec?: Record<string, any>; // カスタムツールの実行仕様（kind!=builtin）
  deferred?: boolean; // schema を遅延する（SearchExtraTools/ExecuteExtraTool）
  calls?: number; // persistent runtime invocation count (older APIs may omit it)
}

// ---- Stats ----
export interface Stats {
  assets: number;
  engine_mode: EngineMode;
  llm_configured: boolean;
  roe_enabled: boolean;
  findings_confirmed: number;
  active_task?: Partial<Task>;
}

// ---- Intercept Rules ----
export type InterceptAction = "allow" | "deny" | "ask";
export type InterceptMatchTarget = "tool_name" | "tool_input";
export type InterceptMatchType = "string" | "regex";

export interface InterceptRule {
  id: number;
  name: string;
  enabled: boolean;
  priority: number;
  match_target: InterceptMatchTarget;
  match_type: InterceptMatchType;
  pattern: string;
  action: InterceptAction;
  message: string;
  timeout_enabled: boolean;
  timeout_seconds: number;
  timeout_action: "deny" | "allow";
  created_at: string;
  updated_at: string;
}

// ---- Asset Intercept Rules（資産インターセプト: グローバルな拒否リスト） ----
export type AssetInterceptKind =
  | "exact_domain"
  | "exact_ip"
  | "exact_url"
  | "fuzzy_domain"
  | "fuzzy_ip"
  | "fuzzy_url"
  | "cidr";

// action はタスク単位のルールだけに使う。block はインターセプト（テスト禁止）、allow は許可（許可リスト）。
export type AssetInterceptAction = "block" | "allow";

// タスク単位の資産インターセプト/許可ルールの入力項目（タスク作成とタスク詳細の編集で使う）。
export interface AssetInterceptRuleInput {
  action: AssetInterceptAction;
  kind: AssetInterceptKind;
  pattern: string;
  note: string;
  enabled: boolean;
}

export interface AssetInterceptRule {
  id: number;
  enabled: boolean;
  action?: AssetInterceptAction; // グローバルルールにはこのフィールドが無い（常にインターセプト）。タスク単位のルールは block と allow を分ける
  kind: AssetInterceptKind;
  pattern: string;
  note: string;
  builtin: boolean;
  created_at: string;
  updated_at: string;
}

export interface InterceptPending {
  decision_source?: "rule" | "model" | "unknown" | "";
  id: number;
  rule_id?: number;
  conversation_id?: number;
  task_id?: string;
  agent_name: string;
  tool_name: string;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  tool_input: Record<string, any>;
  status: "pending" | "allowed" | "denied" | "timeout";
  reason: string; // ルールの message、またはモデル判定の理由（モデル判定は [モデル] 接頭辞付き）
  decided_at?: string;
  created_at: string;
}

// JudgeConfig。モデルによる最終承認（どのインターセプトルールにも当たらないときだけモデルが判断する）のグローバル設定。
export interface JudgeConfig {
  enabled: boolean;
  profile_id: number; // 0 は有効な設定、または既定の設定に従う
  prompt: string; // 判定プロンプト。GET で未設定のとき、バックエンドが組み込みテンプレート全文を戻す
  timeout_seconds: number; // モデル呼び出しのタイムアウト
  fail_action: "allow" | "ask" | "deny"; // モデルのエラー、タイムアウト、解析不能のときの戻り先
  ask_timeout_seconds: number; // モデルが ask と判定して人に回したあとの、承認待ちタイムアウト
  ask_timeout_action: "allow" | "deny"; // 承認がタイムアウトしたあとの既定動作
}

// JudgeUsage。モデルによる最終承認（judge 経路）の累計 token 使用量と、直近 N 日の日次系列。
export interface JudgeDayUsage {
  date: string; // YYYY-MM-DD (UTC)
  calls: number;
  input_tokens: number;
  output_tokens: number;
}
export interface JudgeUsage {
  calls: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  daily: JudgeDayUsage[];
}

export interface InterceptApprovalFilter {
  status?: InterceptPending["status"];
  decision_source?: "rule" | "model" | "unknown";
}

// InterceptApprovalRow enriches InterceptPending with conversation/task and rule context.
export interface InterceptApprovalRow extends InterceptPending {
  conv_title: string; // "" if no linked conversation
  conv_agent_key: string; // "" if no linked conversation
  rule_name: string; // "" if rule was deleted
}

// ── 資産同期 (ScopeSentry データソース) ──────────────────────────────────────────────
export interface SSProject {
  id: string; // MongoDB ObjectID — used as filter.project
  name: string;
  logo?: string;
  AssetCount?: number;
  tag?: string;
}

export interface SSTask {
  id: string;
  name: string; // used as filter.task
  status?: number;
  progress?: number;
  creatTime?: string;
  endTime?: string;
}

// ConvTokenSummary — one conversation's token total (+ profile/date) for merging
// chat usage into the dashboard token stats. GET /api/tokens/conversations.
export interface ConvTokenSummary {
  llm_profile_id: number | null;
  created_at: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// ---- Command recording (Bash execution history) ----
export interface CommandRecord {
  id: number;
  exploration_id: number;
  worker: string;
  tool: string;
  command: string; // raw tool input (JSON)
  output: string;
  is_error: boolean;
  created_at: string;
}

// ツール1つの呼び出し統計（/commands/stats）。errors はそのうち失敗した回数。
export interface ToolStat {
  tool: string;
  total: number;
  errors: number;
}

// ---- LLM recording ----
export interface LLMRecordItem {
  id: number;
  ts: string;
  model: string;
  profile_name: string;
  session_id: string;
  task_id: string;
  worker: string;
  latency_ms: number;
  input_tokens: number;
  output_tokens: number;
  cache_read: number;
  cache_write: number;
  status: string;
  error?: string;
}

export interface LLMRecordDetail extends LLMRecordItem {
  request_body: string;
  response_body: string;
  // provider が実際に送受信した HTTP 原文。リクエストは buildBody() が送った完全な body（ツール
  // schema を含む）。レスポンスは生の SSE フレーム。上の request_body/response_body は正規化ビューで、
  // ツール schema と tool_use ブロックを捨てている。古い記録は空。
  raw_request?: string;
  raw_response?: string;
}

// One distinct task with its LLM-record count (task picker on the records page).
export interface LLMTask {
  task_id: string;
  count: number;
}

// The exact JSON sent to the review model, retained for all model verdicts.
export interface InterceptReviewInput {
  version: number;
  background?: {
    // worker_summary is retained only for immutable v2/v3 snapshots.
    source: "user_message" | "worker_summary";
    text: string;
    truncated?: boolean;
  };
  // Version 1 snapshots are immutable and remain readable in historical audits.
  task?: {
    task_id: number;
    description: string;
    goal: string;
    constraints: { id: number; kind: string; text: string; origin: string; created_at: number }[];
    truncated?: boolean;
  };
  working_directory?: string;
  worker_intent?: string;
  turn_input?: string;
  background_truncated?: boolean;
  // Legacy v1/v2 snapshots only; v3 never sends execution history.
  history?: {
    tool_use_id: string;
    tool: string;
    arguments_preview: string;
    result: string;
    status: "succeeded" | "failed";
    truncated?: boolean;
  }[];
  history_truncated?: boolean;
  correlation?: "exact" | "ambiguous" | "unavailable";
  tool_name: string;
  arguments: Record<string, unknown>;
}

// Immutable review snapshot plus separately recorded execution outcome.
export interface InterceptAudit {
  model_input?: InterceptReviewInput;
  model_input_digest?: string;
  run_id?: string;
  tool_use_id?: string;
  correlation: "exact" | "ambiguous" | "unavailable";
  input_digest: string;
  user_message: string;
  user_truncated?: boolean;
  context:
    | { kind: string; tool?: string; tool_use_id?: string; text: string; is_error?: boolean; truncated?: boolean }[]
    | null;
  context_truncated?: boolean;
  captured_at: string;
  model_fallback?: boolean;
  initial_action: "allow" | "ask" | "deny";
  initial_reason: string;
  effective_action?: "allow" | "deny";
  decision_reason?: string;
  rule_name?: string;
  config_digest?: string;
  profile_id?: number;
  execution_status: "not_started" | "not_executed" | "awaiting_result" | "succeeded" | "failed" | "unknown";
  output?: string;
  output_truncated?: boolean;
  execution_ended_at?: string;
}
export interface InterceptDetail extends InterceptApprovalRow {
  audit: InterceptAudit | null;
}

export type TrafficEvidenceRole = "baseline" | "proof" | "verification" | "supporting";
export interface TrafficEvidenceRef {
  traffic_id: string;
  role?: TrafficEvidenceRole;
  note?: string;
}
export interface TrafficEvidenceSnapshot {
  id: string;
  source_traffic_id: string;
  captured_at: number;
  url: string;
  method: string;
  status: number;
  content_type: string;
  req_head?: string;
  resp_head?: string;
  req_hash: string;
  resp_hash: string;
  req_len: number;
  resp_len: number;
}
export interface FindingTrafficBinding {
  id: string;
  finding_id: string;
  snapshot_id: string;
  role: TrafficEvidenceRole;
  note: string;
  position: number;
  created_at: string;
  snapshot: TrafficEvidenceSnapshot;
}
export interface FindingTraffic {
  finding_id: string;
  version: number;
  report_version: number;
  bindings: FindingTrafficBinding[];
}
export interface EvidenceBodyPreview {
  content: string;
  offset: number;
  total: number;
  next_offset: number;
  truncated: boolean;
  binary: boolean;
}
export interface FindingTrafficDetail {
  binding: FindingTrafficBinding;
  request: EvidenceBodyPreview;
  response: EvidenceBodyPreview;
}

/** GET /api/update/check。現在のバージョンと GitHub の最新正式版との比較結果。 */
export interface UpdateCheck {
  /** いま動いているバージョン。開発ビルドは "dev"、または git describe の接尾辞付き。 */
  current: string;
  /** 実行形態。docker では入れ替えはコンテナの書き込み層だけに効き、コンテナを作り直すとイメージのバージョンに戻る。 */
  mode: "docker" | "binary";
  os: string;
  arch: string;
  repo: string;
  /** ロールバックできる前のバージョン（artex.old）があるか。 */
  has_backup: boolean;
  /** 今回の起動時の自己アップデートの結論（入れ替え失敗、ロールバック済みなど）。何も無ければ空。 */
  boot_notice?: string;
  rolled_back?: boolean;
  /** GitHub の照会に失敗したときの理由。このときは下のフィールドは無い。 */
  error?: string;
  latest?: string;
  notes?: string;
  html_url?: string;
  published_at?: string;
  /** 現在のプラットフォームに対応するリリースパッケージ名と、その Release が本当にそれを含むか。 */
  asset?: string;
  asset_available?: boolean;
  size?: number;
  has_update?: boolean;
  /** 両方のバージョン番号を比較できるか。開発ビルドは false で、このときはワンクリック更新を無効にする。 */
  comparable?: boolean;
  /** comparable が false のときの説明。 */
  reason?: string;
}

/** /api/update/stream が送る更新進捗の1件。 */
export interface UpdateProgress {
  phase: "idle" | "downloading" | "verifying" | "extracting" | "staged" | "failed";
  /** ダウンロード段階だけ意味がある（0-100）。それ以外の段階は -1。 */
  percent: number;
  message: string;
  version?: string;
  error?: string;
}

// Original execution selected from an approval, never submitted to the reviewer.
export interface InterceptExecution {
  conversation_id: number | null;
  task_id: string | null;
  session: string;
  seq: number;
  items: Activity[];
}
