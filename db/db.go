// Package db is the PostgreSQL data source for ARTEX (旧 graph の単一ファイル SQLite に代わる)。
// 接続を開き、schema を適用し、組み込み agent と変数カタログを seed する。
package db

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Autumn-27/artex/config"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // pgx database/sql driver ("pgx")
)

//go:embed schema.sql
var schemaSQL string

const schemaMigrationLockKey int64 = 7337741001

// maxOpenConns は接続プールの上限。説明は Open にある。値は PostgreSQL 既定の
// max_connections=100 よりかなり小さく、同時にアプリ自身が接続を入れ子で取る深さよりかなり大きい
// （起動時の schema の advisory lock は接続を 1 本持ったまま、seedBuiltins が別の接続を取る）。自己デッドロックはしない。
const maxOpenConns = 32

var schemaDeadlockRetryDelays = [...]time.Duration{
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	time.Second,
}

type schemaExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func isPostgresDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

func applySchemaWithRetry(ctx context.Context, execer schemaExecer, sleep func(time.Duration)) error {
	for attempt := 0; ; attempt++ {
		if _, err := execer.ExecContext(ctx, schemaSQL); err != nil {
			if !isPostgresDeadlock(err) || attempt >= len(schemaDeadlockRetryDelays) {
				return err
			}
			sleep(schemaDeadlockRetryDelays[attempt])
			continue
		}
		return nil
	}
}

// withSchemaMigrationLock pins the session-level lock to one checked-out
// connection. Running pg_advisory_lock through *sql.DB is incorrect because a
// later schema or unlock call may use a different pooled PostgreSQL session.
func withSchemaMigrationLock(ctx context.Context, sqlDB *sql.DB, action func(*sql.Conn) error) (err error) {
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, schemaMigrationLockKey); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	defer func() {
		if _, unlockErr := conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, schemaMigrationLockKey); unlockErr != nil && err == nil {
			err = fmt.Errorf("advisory unlock: %w", unlockErr)
		}
	}()
	return action(conn)
}

// coordinateWithSchemaMigration makes long, multi-table archive transactions
// mutually exclusive with startup DDL while allowing ordinary runtime queries
// to continue normally.
func coordinateWithSchemaMigration(tx *sql.Tx) error {
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, schemaMigrationLockKey); err != nil {
		return fmt.Errorf("coordinate with schema migration: %w", err)
	}
	return nil
}

// DSN resolves the PostgreSQL connection string and reports where it came from.
// Precedence: env ARTEX_PG_DSN > config file (config.json). There is no
// built-in default — it errors if neither source is configured.
func DSN() (dsn, source string, err error) {
	return config.PostgresDSN()
}

// DB wraps the shared *sql.DB. PG handles its own connection pool + concurrency
// (MVCC), so unlike the old SQLite store there is no process-wide write mutex.
type DB struct{ *sql.DB }

// ensureDatabase connects to the postgres system database and creates the target
// database if it does not exist. dsn must be a postgres:// URL.
func ensureDatabase(dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil // unparseable DSN — let the normal Open fail with a clear error
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if dbName == "" || dbName == "postgres" {
		return nil
	}
	// connect to the postgres maintenance database instead
	adminDSN := *u
	adminDSN.Path = "/postgres"
	admin, err := sql.Open("pgx", adminDSN.String())
	if err != nil {
		return nil // best-effort; let Open surface the real error
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		return nil
	}
	var exists bool
	_ = admin.QueryRow(`SELECT true FROM pg_database WHERE datname=$1`, dbName).Scan(&exists)
	if !exists {
		if _, err := admin.Exec(`CREATE DATABASE "` + dbName + `"`); err != nil {
			return fmt.Errorf("create database %q: %w", dbName, err)
		}
	}
	return nil
}

// Open connects, applies the schema (idempotent), and seeds builtin rows.
func Open(dsn string) (*DB, error) {
	if err := ensureDatabase(dsn); err != nil {
		return nil, err
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	// database/sql は既定で接続数を制限しない。プールに空きが無いと無条件に新規接続を作り、
	// PostgreSQL の max_connections（既定 100）まで行って初めて拒否される。ピーク時のクエリが受け取るのは
	// `FATAL: sorry, too many clients already` というエラーである。上限を付けると、超過したクエリは
	// 空き接続を待つ列に入る。同じ負荷でも、エラーにならず遅くなる。呼び出し側は
	// 「読めない」と「無い」を区別しなくてよい。maxOpenConns は psql / reset-password.sh と、
	// 並存し得るほかのインスタンスの分を残す。max_connections を下げた場合は、ここも合わせて下げる。
	sqlDB.SetMaxOpenConns(maxOpenConns)
	sqlDB.SetMaxIdleConns(maxOpenConns)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping postgres (%s): %w", config.Redact(dsn), err)
	}
	d := &DB{sqlDB}
	// pgx runs multi-statement Exec via the simple protocol when there are no args.
	// Keep the dedicated lock connection checked out until both DDL and seeding
	// finish so concurrent application instances cannot initialize out of order.
	err = withSchemaMigrationLock(context.Background(), sqlDB, func(conn *sql.Conn) error {
		if err := applySchemaWithRetry(context.Background(), conn, time.Sleep); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		if err := d.seedBuiltins(); err != nil {
			return fmt.Errorf("seed builtins: %w", err)
		}
		return nil
	})
	if err != nil {
		sqlDB.Close()
		return nil, err
	}
	return d, nil
}

// builtinAgent describes one of the fixed agents and its prompt-variable catalog.
type builtinAgent struct {
	key, name, role, desc string
	vars                  []promptVar
	interactiveShell      bool // 行を作るときの既定の対話式 shell。ON CONFLICT は、ユーザーがその後手で切り替えた値を上書きしない
	runSeconds            *int // 行を作るときの 1 回の run の壁時計上限（秒）。nil は seed の既定（1200）。0 は時間制限なし
}

type promptVar struct{ name, desc, example, source string }

// intp は v のポインタを返す。builtinAgent の任意フィールド（runSeconds など）に明示的な値を入れる。
func intp(v int) *int { return &v }

// builtinAgents mirrors docs §5(a). 組み込みツールは DB に入れない。ここでは agent と変数カタログだけを seed する。
// planner/worker/mainagent/auto の対話式 shell の既定は、下の interactive_shell_default_v1
// ブロックでまとめて true にする（その後の toggle は尊重する）。ここの interactiveShell は、「行を作った時点で既定オン」が要る新しい agent だけに使う。
var builtinAgents = []builtinAgent{
	{"goals", "目標分解", "goals", "ペネトレーションテストのタスク目標を、独立して検証できる複数のサブ目標に分解します。", []promptVar{
		{"EngagementDescription", "タスクの説明（テスト対象/背景）", "example.com のサイトをテストする", "exploration"},
		// Now はグローバルな runtime 変数である（server.globalPromptVars を参照）。各 agent のカタログでは
		// 重ねて定義しない。withGlobalVars が足したときに、グローバル側と名前が衝突する。
	}, false, nil},
	{"planner", "プランナー", "planner", "状況を読み取って目標を判定し、本当に未カバーの新しい方向があるときだけ探索インテントを補います（タスクごとに計画ループが 1 つ）。", []promptVar{
		{"Goal", "タスクの全体目標", "example.com の管理者権限を取得する", "exploration"},
		{"AssetSummary", "資産数/種別分布の要約(任意)", "domain:3 ip:5 site:2", "distilled"},
	}, false, nil},
	{"mainagent", "メイン", "main", "人との接点：進捗を観察し、人の意図を hint または優先度の高いインテントに落とし込みます。", []promptVar{
		{"Goal", "現在のタスクの目標", "example.com の管理者権限を取得する", "exploration"},
		{"AssetSummary", "開始時の状況の要約(任意)", "domain:3 ip:5", "distilled"},
		{"FindingsSummary", "確認済みの脆弱性の要約(任意)", "high:1 medium:2", "distilled"},
	}, false, nil},
	{"worker", "ワーカー", "worker", "インテントを 1 件受け取って実行し、見つけた事実/脆弱性を知識グラフに書き戻したら停止します。", []promptVar{
		{"ProxyAddr", "記録プロキシのアドレス(if による 2 通りの文言を切り替え)", "127.0.0.1:8080", "runtime"},
		{"WorkerName", "worker の自己識別名(任意)", "worker-1", "runtime"},
	}, false, nil},
	// Auto。組み込みの「プラットフォーム操作」agent。ペネトレーションのオーケストレーションループには入らない。対話ページから動かし、ツールでプラットフォームを操作する。
	{"auto", "Auto", "assistant", "プラットフォーム操作アシスタント：ツールでタスク(作成/閲覧/一時停止/ヒント付与)と資産を管理し、skill、カスタムツール、MCP の作成や変更もできます。", nil, false, nil},
	// ペネトレーションテスト。組み込みの「独立ペネトレーション」agent。対話ページから動かし、1 人で偵察から締めまで一連の流れを進める。計画、実行、検証は自分で行う。対話式 shell は既定でオン。
	{"pentest", "ペネトレーションテスト", "assistant", "独立型ペネトレーション agent：偵察→攻撃面の特定→深掘り→検証→まとめまでの一連の流れを 1 つで進め、計画、実行、敵対的な検証を自分で行います。", nil, true, intp(0)},
}

// seedBuiltins inserts the fixed built-in agents and their variable catalog (idempotent).
func (d *DB) seedBuiltins() error {
	for _, a := range builtinAgents {
		var agentID int64
		err := d.QueryRow(`
INSERT INTO agents(key, name, description, role, builtin, enabled, interactive_shell, run_seconds)
VALUES ($1, $2, NULLIF($3,''), $4, true, true, $5, COALESCE($6, 1200))
ON CONFLICT (key) DO UPDATE SET name = EXCLUDED.name, description = EXCLUDED.description
RETURNING id`, a.key, a.name, a.desc, a.role, a.interactiveShell, a.runSeconds).Scan(&agentID)
		if err != nil {
			return fmt.Errorf("agent %s: %w", a.key, err)
		}
		for _, v := range a.vars {
			if _, err := d.Exec(`
INSERT INTO agent_prompt_vars(agent_id, var_name, description, example, source)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (agent_id, var_name) DO UPDATE
  SET description = EXCLUDED.description, example = EXCLUDED.example, source = EXCLUDED.source`,
				agentID, v.name, v.desc, v.example, v.source); err != nil {
				return fmt.Errorf("agent %s var %s: %w", a.key, v.name, err)
			}
		}
	}
	// Drop catalog entries for variables that were renamed, so the white-list no
	// longer advertises a name templates can't resolve (EngagementTitle→Description).
	// 'Now' を各 agent のカタログからグローバルな runtime 変数へ上げたあと、古い庫の goals には 'Now' が 1 件残る。
	// グローバル側と名前が衝突する（フロントの変数一覧で key が重複する）。合わせて消す。
	if _, err := d.Exec(`DELETE FROM agent_prompt_vars WHERE var_name IN ('EngagementTitle', 'CoverageGaps', 'Now')`); err != nil {
		return fmt.Errorf("cleanup renamed vars: %w", err)
	}
	// Default-on interactive_shell for the runtime agents (planner/worker/mainagent/auto)
	// ONCE — respects a later user toggle-off (guarded by a settings flag). goals(one-shot
	// decomposer) stays off. Runs after the column exists (schema applied before seed).
	if v, _, _ := d.GetSetting("interactive_shell_default_v1"); v != "true" {
		if _, err := d.Exec(`UPDATE agents SET interactive_shell=true WHERE key IN ('planner','worker','mainagent','auto')`); err != nil {
			return fmt.Errorf("seed interactive_shell defaults: %w", err)
		}
		_ = d.SetSetting("interactive_shell_default_v1", "true")
	}
	// Seed the built-in browser (Playwright) MCP once — DISABLED by default (ユーザーが
	// 必要なときに自分で有効にする), no proxy by default. The traffic-capture toggle injects/strips
	// the recording proxy + CA at runtime (server.Manager.syncBrowserMCPProxy).
	// Insert only if absent so we never clobber user edits (args/env/enabled/
	// visibility) on restart. The packaged image installs the pinned
	// playwright-mcp binary, so use it directly instead of npx (no registry
	// lookup, no accidental latest-version download).
	if _, err := d.Exec(`
INSERT INTO mcp_servers(name, transport, command, args, env, enabled)
VALUES ('browser', 'stdio', 'playwright-mcp', $1, '{}', false)
ON CONFLICT (name) DO NOTHING`,
		`["--headless","--no-sandbox"]`); err != nil {
		return fmt.Errorf("seed browser mcp: %w", err)
	}
	// Migrate the upstream default row from npx to the pinned binary. Only the
	// exact default is touched; user-edited commands and args are left alone.
	if _, err := d.Exec(`
UPDATE mcp_servers
SET command='playwright-mcp', args='["--headless","--no-sandbox"]'::jsonb
WHERE name='browser' AND command='npx'
  AND args='["@playwright/mcp","--headless"]'::jsonb`); err != nil {
		return fmt.Errorf("migrate browser mcp: %w", err)
	}
	// NOTE: the placeholder ScopeSentry data-source MCP (empty URL + empty X-API-Key,
	// disabled) is seeded directly in schema.sql §F so a raw `psql < schema.sql` init
	// also gets it. schema.sql is Exec'd on every startup, so it stays idempotent.
	if err := d.seedBuiltinSkillVisibility(); err != nil {
		return fmt.Errorf("seed skill visibility: %w", err)
	}
	if err := d.renameBuiltinRules(); err != nil {
		return err
	}
	if err := d.seedDefaultInterceptRules(); err != nil {
		return fmt.Errorf("seed intercept rules: %w", err)
	}
	if err := d.seedDefaultInterceptRulesV2(); err != nil {
		return fmt.Errorf("seed intercept rules v2: %w", err)
	}
	if err := d.seedDefaultInterceptRulesV3(); err != nil {
		return fmt.Errorf("seed intercept rules v3: %w", err)
	}
	if err := d.seedDefaultAssetInterceptRules(); err != nil {
		return fmt.Errorf("seed asset intercept rules: %w", err)
	}
	return nil
}

// builtinRuleRenames maps the former Chinese built-in rule labels to their Japanese
// replacements so databases seeded before the translation are renamed in place
// (the seeds run once, so they never rewrite existing rows).
var builtinRuleRenames = [][2]string{
	{"[内置] 政府网站 (.gov)", "[組み込み] 政府系サイト (.gov)"},
	{"[内置] 政府网站 (.gov.cn)", "[組み込み] 政府系サイト (.gov.cn)"},
	{"[内置] 教育网站 (.edu)", "[組み込み] 教育系サイト (.edu)"},
	{"[内置] 教育网站 (.edu.cn)", "[組み込み] 教育系サイト (.edu.cn)"},
	{"[内置] 递归强制删除 rm -rf", "[組み込み] 再帰的な強制削除 rm -rf"},
	{"[内置] 删除系统关键目录", "[組み込み] システムの重要ディレクトリの削除"},
	{"[内置] 磁盘格式化 mkfs", "[組み込み] ディスクのフォーマット mkfs"},
	{"[内置] 覆写磁盘设备 dd", "[組み込み] ディスクデバイスの上書き dd"},
	{"[内置] Fork 炸弹", "[組み込み] Fork ボム"},
	{"[内置] 关机 / 重启", "[組み込み] シャットダウン / 再起動"},
	{"[内置] 杀死全部进程", "[組み込み] 全プロセスの強制終了"},
	{"[内置] 磁盘擦除 shred / wipe", "[組み込み] ディスクの消去 shred / wipe"},
	{"[内置] 清空防火墙规则", "[組み込み] ファイアウォールルールの全削除"},
	{"[内置] SQL DROP DATABASE / TABLE / SCHEMA", "[組み込み] SQL DROP DATABASE / TABLE / SCHEMA"},
	{"[内置] SQL TRUNCATE", "[組み込み] SQL TRUNCATE"},
	{"[内置] MongoDB drop / dropDatabase", "[組み込み] MongoDB drop / dropDatabase"},
	{"[内置] Redis FLUSHALL / FLUSHDB", "[組み込み] Redis FLUSHALL / FLUSHDB"},
	{"[内置] curl / wget 发送 DELETE 请求", "[組み込み] curl / wget による DELETE リクエスト送信"},
	{"[内置] Python HTTP 客户端 DELETE（requests/httpx/aiohttp）", "[組み込み] Python HTTP クライアントの DELETE（requests/httpx/aiohttp）"},
	{"[内置] 脚本中声明 HTTP DELETE 方法（JS/通用）", "[組み込み] スクリプト内での HTTP DELETE メソッドの宣言（JS/汎用）"},
	{"[内置] 批量清空 / 清除接口路径", "[組み込み] 一括クリア / 削除系のエンドポイントパス"},
	{"[内置] 破坏性系统命令", "[組み込み] 破壊的なシステムコマンド"},
	{"[内置] 数据外泄管道", "[組み込み] データ流出パイプ"},
	{"[内置] 删除类接口路径", "[組み込み] 削除系のエンドポイントパス"},
}

// builtinRuleMessageRenames does the same for the rejection messages: only rows whose
// message is still the original Chinese text are rewritten, so user edits survive.
var builtinRuleMessageRenames = [][2]string{
	{"禁止执行递归强制删除（rm -rf / rm --recursive），可能永久损坏系统或靶机环境", "再帰的な強制削除（rm -rf / rm --recursive）の実行は禁止です。システムや検証対象の環境を恒久的に破損する恐れがあります"},
	{"禁止删除系统关键路径", "システムの重要なパスの削除は禁止です"},
	{"禁止格式化磁盘（mkfs）", "ディスクのフォーマット（mkfs）は禁止です"},
	{"禁止使用 dd 覆写磁盘设备", "dd によるディスクデバイスの上書きは禁止です"},
	{"禁止执行 Fork 炸弹", "Fork ボムの実行は禁止です"},
	{"禁止执行关机或重启命令", "シャットダウンや再起動のコマンドの実行は禁止です"},
	{"禁止 kill -9 -1 或 killall -9（杀死所有进程）", "kill -9 -1 や killall -9（全プロセスの強制終了）は禁止です"},
	{"禁止对磁盘设备执行 shred/wipe 擦除", "ディスクデバイスへの shred/wipe による消去は禁止です"},
	{"禁止清空防火墙规则（iptables -F / nft flush）", "ファイアウォールルールの全削除（iptables -F / nft flush）は禁止です"},
	{"禁止执行 DROP 操作，可能不可逆地销毁数据库对象", "DROP の実行は禁止です。データベースのオブジェクトを元に戻せない形で破壊する恐れがあります"},
	{"禁止执行 TRUNCATE，可能清空数据表所有数据", "TRUNCATE の実行は禁止です。テーブルの全データが消える恐れがあります"},
	{"禁止执行 MongoDB drop 操作", "MongoDB の drop 操作の実行は禁止です"},
	{"禁止执行 Redis FLUSHALL / FLUSHDB，可能清空全部缓存数据", "Redis の FLUSHALL / FLUSHDB の実行は禁止です。キャッシュデータがすべて消える恐れがあります"},
	{"禁止通过 curl/wget 发送 HTTP DELETE 请求，可能删除目标系统数据", "curl/wget による HTTP DELETE リクエストの送信は禁止です。対象システムのデータが削除される恐れがあります"},
	{"禁止使用 Python HTTP 客户端发送 DELETE 请求", "Python の HTTP クライアントによる DELETE リクエストの送信は禁止です"},
	{"禁止在脚本中声明并发送 HTTP DELETE 请求", "スクリプト内での HTTP DELETE リクエストの宣言と送信は禁止です"},
	{"禁止调用批量清空或销毁类接口（/clear /wipe /flush /purge 等）", "一括クリアや破棄を行うエンドポイント（/clear /wipe /flush /purge など）の呼び出しは禁止です"},
	{"破坏性命令被拒绝（rm -rf / / mkfs / dd / fork bomb / 关机重启 / 覆写磁盘设备）", "破壊的なコマンドは拒否されました（rm -rf / / mkfs / dd / fork bomb / シャットダウン・再起動 / ディスクデバイスの上書き）"},
	{"疑似数据外泄管道被拒绝（命令输出经 curl/wget/nc 外传）", "データ流出の疑いがあるパイプは拒否されました（コマンドの出力が curl/wget/nc で外部に送られます）"},
	{"禁止调用删除类接口（/delete /remove /unlink /erase 等），不论使用哪种 HTTP 方法——多数应用的删除接口用 GET/POST 就能触发，同样会真实删除目标数据", "削除系のエンドポイント（/delete /remove /unlink /erase など）の呼び出しは、HTTP メソッドを問わず禁止です。多くのアプリでは削除用のエンドポイントが GET/POST でも動作し、同じく対象のデータが実際に削除されます"},
}

// renameBuiltinRules is idempotent: it only touches rows still carrying the old label.
func (d *DB) renameBuiltinRules() error {
	for _, p := range builtinRuleMessageRenames {
		if _, err := d.Exec(`UPDATE intercept_rules SET message = $2 WHERE message = $1`, p[0], p[1]); err != nil {
			return fmt.Errorf("rename intercept rule message %q: %w", p[0], err)
		}
	}
	for _, p := range builtinRuleRenames {
		if _, err := d.Exec(`UPDATE intercept_rules SET name = $2 WHERE name = $1`, p[0], p[1]); err != nil {
			return fmt.Errorf("rename intercept rule %q: %w", p[0], err)
		}
		if _, err := d.Exec(`UPDATE asset_intercept_rules SET note = $2 WHERE builtin AND note = $1`, p[0], p[1]); err != nil {
			return fmt.Errorf("rename asset intercept rule %q: %w", p[0], err)
		}
	}
	return nil
}

// seedDefaultAssetInterceptRules inserts the built-in asset blocklist (fuzzy
// domain matches for government / education sites) once on first startup. Gated
// by a settings flag so a user's later disable/delete is never resurrected on
// restart — same policy as the intercept-rule seed.
func (d *DB) seedDefaultAssetInterceptRules() error {
	if v, _, _ := d.GetSetting("asset_intercept_default_rules_v1"); v == "done" {
		return nil
	}
	rules := []struct {
		kind    string
		pattern string
		note    string
	}{
		{"fuzzy_domain", ".gov", "[組み込み] 政府系サイト (.gov)"},
		{"fuzzy_domain", ".gov.cn", "[組み込み] 政府系サイト (.gov.cn)"},
		{"fuzzy_domain", ".edu", "[組み込み] 教育系サイト (.edu)"},
		{"fuzzy_domain", ".edu.cn", "[組み込み] 教育系サイト (.edu.cn)"},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO asset_intercept_rules(enabled, kind, pattern, note, builtin)
VALUES (true, $1, $2, $3, true)
ON CONFLICT DO NOTHING`, r.kind, r.pattern, r.note); err != nil {
			return fmt.Errorf("asset rule %q: %w", r.pattern, err)
		}
	}
	return d.SetSetting("asset_intercept_default_rules_v1", "done")
}

// builtinSkillVisibility maps a shipped skill's directory name → the built-in
// agent keys that should see it by default. The skill FILES themselves live on the
// filesystem (SkillDir, loaded by norma at runtime); DB only carries this visibility
// binding. Skills omitted here (e.g. playwright-cli, scopesentry) ship invisible by
// default — the user turns them on per-agent when needed. scopesentry additionally
// declares `mcps: ScopeSentry`, which only takes effect once it's made visible and
// that MCP is enabled/configured.
var builtinSkillVisibility = map[string][]string{
	"api-recon": {"auto", "pentest", "worker"},
}

// seedBuiltinSkillVisibility binds the shipped built-in skills to their default
// agents. Insert-if-absent (ON CONFLICT DO NOTHING) so a user's later toggle-off is
// never resurrected on restart — matches the browser-MCP / intercept-rule seed policy.
func (d *DB) seedBuiltinSkillVisibility() error {
	for skillName, agentKeys := range builtinSkillVisibility {
		for _, key := range agentKeys {
			if _, err := d.Exec(`
INSERT INTO agent_skill_visibility(agent_id, skill_name, enabled)
SELECT id, $2, true FROM agents WHERE key=$1
ON CONFLICT (agent_id, skill_name) DO NOTHING`, key, skillName); err != nil {
				return fmt.Errorf("skill %s → agent %s: %w", skillName, key, err)
			}
		}
	}
	return nil
}

// seedDefaultInterceptRules inserts built-in safety intercept rules once on
// first startup. The seed is gated by a settings flag so user edits (disable,
// delete, re-order) are never overwritten on subsequent restarts.
func (d *DB) seedDefaultInterceptRules() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v1"); v == "done" {
		return nil
	}
	type rule struct {
		name     string
		target   string // tool_name | tool_input
		typ      string // string | regex
		pattern  string
		action   string
		message  string
		priority int
	}
	rules := []rule{
		// ── システムの破壊的コマンド (priority 100) ──────────────────────────────────
		{
			name:     "[組み込み] 再帰的な強制削除 rm -rf",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\brm\b.{0,80}(?:-[a-z]*r[a-z]*f[a-z]*|-[a-z]*f[a-z]*r[a-z]*|--recursive|--no-preserve-root)`,
			action:   "deny",
			message:  "再帰的な強制削除（rm -rf / rm --recursive）の実行は禁止です。システムや検証対象の環境を恒久的に破損する恐れがあります",
			priority: 100,
		},
		{
			name:     "[組み込み] システムの重要ディレクトリの削除",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\brm\b[^"'\n]{0,60}["'\s](/|/etc|/bin|/usr|/boot|/var|/lib|/sys|/proc|/dev|/sbin|/root)`,
			action:   "deny",
			message:  "システムの重要なパスの削除は禁止です",
			priority: 100,
		},
		{
			name:     "[組み込み] ディスクのフォーマット mkfs",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bmkfs\b`,
			action:   "deny",
			message:  "ディスクのフォーマット（mkfs）は禁止です",
			priority: 100,
		},
		{
			name:     "[組み込み] ディスクデバイスの上書き dd",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bdd\b[^|\n]{0,100}\bof=\s*/dev/[a-zA-Z]`,
			action:   "deny",
			message:  "dd によるディスクデバイスの上書きは禁止です",
			priority: 100,
		},
		{
			name:     "[組み込み] Fork ボム",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `:\(\)\s*\{[^}]*:\|:`,
			action:   "deny",
			message:  "Fork ボムの実行は禁止です",
			priority: 100,
		},
		{
			name:     "[組み込み] シャットダウン / 再起動",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\b(?:shutdown|reboot|halt|poweroff|init\s+[06])\b`,
			action:   "deny",
			message:  "シャットダウンや再起動のコマンドの実行は禁止です",
			priority: 100,
		},
		{
			name:     "[組み込み] 全プロセスの強制終了",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bkill\s+-9\s+-1\b|\bkillall\s+-9\b`,
			action:   "deny",
			message:  "kill -9 -1 や killall -9（全プロセスの強制終了）は禁止です",
			priority: 100,
		},
		{
			name:     "[組み込み] ディスクの消去 shred / wipe",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\b(?:shred|wipe)\b[^|\n]{0,80}/dev/[a-zA-Z]`,
			action:   "deny",
			message:  "ディスクデバイスへの shred/wipe による消去は禁止です",
			priority: 100,
		},
		{
			name:     "[組み込み] ファイアウォールルールの全削除",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\biptables\s+(?:-F|--flush)\b|\bnft\s+flush\s+ruleset\b`,
			action:   "deny",
			message:  "ファイアウォールルールの全削除（iptables -F / nft flush）は禁止です",
			priority: 100,
		},
		// ── データベースの破壊的操作 (priority 90) ─────────────────────────────────
		{
			name:     "[組み込み] SQL DROP DATABASE / TABLE / SCHEMA",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bDROP\s+(?:DATABASE|TABLE|SCHEMA|INDEX|VIEW|TABLESPACE|USER|ROLE)\b`,
			action:   "deny",
			message:  "DROP の実行は禁止です。データベースのオブジェクトを元に戻せない形で破壊する恐れがあります",
			priority: 90,
		},
		{
			name:     "[組み込み] SQL TRUNCATE",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bTRUNCATE\s+(?:TABLE\s+)?\w`,
			action:   "deny",
			message:  "TRUNCATE の実行は禁止です。テーブルの全データが消える恐れがあります",
			priority: 90,
		},
		{
			name:     "[組み込み] MongoDB drop / dropDatabase",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\.(?:dropDatabase|dropCollection|drop)\s*\(`,
			action:   "deny",
			message:  "MongoDB の drop 操作の実行は禁止です",
			priority: 90,
		},
		{
			name:     "[組み込み] Redis FLUSHALL / FLUSHDB",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\b(?:FLUSHALL|FLUSHDB)\b`,
			action:   "deny",
			message:  "Redis の FLUSHALL / FLUSHDB の実行は禁止です。キャッシュデータがすべて消える恐れがあります",
			priority: 90,
		},
		// ── HTTP の破壊的リクエスト (priority 80) ──────────────────────────────────
		// Agent が DELETE リクエストを送る、よくある 3 通り。
		//   1. curl -X DELETE / --request DELETE（Bash ツールが直接実行するか、スクリプトへ書く）
		//   2. Python HTTP クライアントの .delete() メソッド
		//   3. JS や汎用スクリプトの method: 'DELETE' / method="DELETE"
		{
			name:     "[組み込み] curl / wget による DELETE リクエスト送信",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bcurl\b[^|\n&;"]{0,300}(?:-X\s*DELETE|--request\s+DELETE|-XDELETE)|\bwget\b[^|\n&;"]{0,300}--method[=\s]+DELETE`,
			action:   "deny",
			message:  "curl/wget による HTTP DELETE リクエストの送信は禁止です。対象システムのデータが削除される恐れがあります",
			priority: 80,
		},
		{
			name:     "[組み込み] Python HTTP クライアントの DELETE（requests/httpx/aiohttp）",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\b(?:requests|httpx|aiohttp|urllib\.request)\.delete\s*\(|session\.delete\s*\(|client\.delete\s*\(`,
			action:   "deny",
			message:  "Python の HTTP クライアントによる DELETE リクエストの送信は禁止です",
			priority: 80,
		},
		{
			name:     "[組み込み] スクリプト内での HTTP DELETE メソッドの宣言（JS/汎用）",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)axios\.delete\s*\(|method\s*[:=]\s*['"]DELETE['"]`,
			action:   "deny",
			message:  "スクリプト内での HTTP DELETE リクエストの宣言と送信は禁止です",
			priority: 80,
		},
		{
			name:     "[組み込み] 一括クリア / 削除系のエンドポイントパス",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)/(?:clear|wipe|flush|purge|truncate|drop|destroy|factory[-_]reset|reset[-_]all)(?:[/?#"'\s]|$)`,
			action:   "deny",
			message:  "一括クリアや破棄を行うエンドポイント（/clear /wipe /flush /purge など）の呼び出しは禁止です",
			priority: 80,
		},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
VALUES ($1, true, $2, $3, $4, $5, $6, $7, false, 60, 'deny')
ON CONFLICT DO NOTHING`,
			r.name, r.priority, r.target, r.typ, r.pattern, r.action, r.message,
		); err != nil {
			return fmt.Errorf("rule %q: %w", r.name, err)
		}
	}
	return d.SetSetting("intercept_default_rules_v1", "done")
}

// seedDefaultInterceptRulesV2 migrates the two safety patterns that used to be
// hard-coded in guard.go (destructive shell + data-exfil pipe) into ordinary
// intercept rules. Gated by its own flag so it also lands on DBs that already ran
// v1. Unlike the old guard.go floor, these are plain [組み込み] rules — the user can
// disable or delete them. The exfil rule ships DISABLED by default (its
// curl/wget/nc pipe pattern mis-fires on legitimate CTF/pentest reverse-shell and
// data-transfer pipes); enable it manually when exfil gating is actually wanted.
func (d *DB) seedDefaultInterceptRulesV2() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v2"); v == "done" {
		return nil
	}
	rules := []struct {
		name     string
		pattern  string
		action   string
		message  string
		enabled  bool
		priority int
	}{
		{
			name:     "[組み込み] 破壊的なシステムコマンド",
			pattern:  `(?i)\b(rm\s+-rf\s+/|mkfs|dd\s+if=|:\(\)\s*\{|shutdown|reboot|>\s*/dev/sd)`,
			action:   "deny",
			message:  "破壊的なコマンドは拒否されました（rm -rf / / mkfs / dd / fork bomb / シャットダウン・再起動 / ディスクデバイスの上書き）",
			enabled:  true,
			priority: 100,
		},
		{
			name:     "[組み込み] データ流出パイプ",
			pattern:  `(?i)(curl|wget|nc|ncat)\b[^|]*\b(\|\s*(curl|wget|nc))`,
			action:   "deny",
			message:  "データ流出の疑いがあるパイプは拒否されました（コマンドの出力が curl/wget/nc で外部に送られます）",
			enabled:  false,
			priority: 80,
		},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
VALUES ($1, $2, $3, 'tool_input', 'regex', $4, $5, $6, false, 60, 'deny')
ON CONFLICT DO NOTHING`,
			r.name, r.enabled, r.priority, r.pattern, r.action, r.message,
		); err != nil {
			return fmt.Errorf("rule %q: %w", r.name, err)
		}
	}
	return d.SetSetting("intercept_default_rules_v2", "done")
}

// seedDefaultInterceptRulesV3 adds the delete-endpoint path rule. The v1 HTTP rules
// only catch the DELETE *method* (curl -X DELETE, requests.delete(, method:'DELETE'),
// and v1's path rule covers only /clear /wipe /flush /purge /truncate /drop /destroy
// /factory-reset /reset-all — so a plain `curl 'http://t/api/user/delete?id=1'` (a
// delete endpoint reached with GET/POST, which is how most web apps expose deletion)
// slipped through every built-in rule. Own flag so it also lands on DBs that already
// ran v1/v2, where editing the v1 seed would have no effect.
//
// The pattern deliberately requires a separator after the verb so /delivery,
// /details, /delta and /delegate do not match, while /deleteAll, /delete_user and
// /delete-user do. destroy is re-covered here because v1's rule does not allow a
// suffix (/destroyAll was missed).
//
// Exported as a package const only so the seeded regex is unit-testable without a DB.
const deleteEndpointPathPattern = `(?i)/(?:(?:delete|remove|unlink|erase|destroy)[-\w]*|del)(?:[/?#"'\s]|$)`

func (d *DB) seedDefaultInterceptRulesV3() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v3"); v == "done" {
		return nil
	}
	const name = "[組み込み] 削除系のエンドポイントパス"
	if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
SELECT $1, true, 80, 'tool_input', 'regex', $2, 'deny', $3, false, 60, 'deny'
WHERE NOT EXISTS (SELECT 1 FROM intercept_rules WHERE name = $1)`,
		name,
		deleteEndpointPathPattern,
		"削除系のエンドポイント（/delete /remove /unlink /erase など）の呼び出しは、HTTP メソッドを問わず禁止です。多くのアプリでは削除用のエンドポイントが GET/POST でも動作し、同じく対象のデータが実際に削除されます",
	); err != nil {
		return fmt.Errorf("rule %q: %w", name, err)
	}
	return d.SetSetting("intercept_default_rules_v3", "done")
}
