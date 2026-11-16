#!/usr/bin/env bash
# =============================================================================
# ARTEX 管理者パスワードのリセットスクリプト
#
# ログインのユーザー名は ARTEX で固定です。パスワードは bcrypt ハッシュとして
# データベース settings テーブルの auth.password_hash キーに置きます。
# このスクリプトはデータベースへ接続し、pgcrypto でライブラリ内に bcrypt ハッシュを作り、
# そのキーへ書き戻します。バックエンドのログイン検証（golang.org/x/crypto/bcrypt）と互換です。
#
# デプロイは二通りです。
#   local （既定）: ホストから psql で直接接続します。接続情報の優先順位は次のとおりです。
#                    コマンド引数 > --dsn/$ARTEX_PG_DSN > config.json の database.*
#   docker        : `docker compose exec`（または `docker exec`）で postgres
#                    コンテナ内の psql を実行します（compose は既定でホストへ 5432 を出さないため、コンテナ内を使います）。
#
# 使い方の例:
#   ./reset-password.sh                          # ローカル。config.json か環境を自動で読み、新しいパスワードを対話入力する
#   ./reset-password.sh -p 'NewPass!'            # ローカル。新しいパスワードを直接指定する
#   ./reset-password.sh --dsn postgres://u:p@h:5432/artex
#   ./reset-password.sh -H 127.0.0.1 -P 5433 -U autopentest -W pass -d artex
#   ./reset-password.sh -m docker                # docker デプロイ（.env の POSTGRES_* を読む）
#   ./reset-password.sh -m docker -c pgコンテナ名 --exec docker
#
# 安全: 新しいパスワードは環境変数と psql の \getenv で渡します（プロセスの argv には入りません）。
# :'var' で自動エスケープします（SQL インジェクションを防ぎます）。データベースのパスワードは
# PGPASSWORD で渡し、同じく argv には入りません。
# =============================================================================
set -euo pipefail

PASS_KEY="auth.password_hash"
BCRYPT_COST=10

MODE=""            # local | docker（空なら自動判定）
DSN=""
HOST="" PORT="" USER="" DBPASS="" DBNAME="" SSLMODE=""
CONFIG=""
CONTAINER=""       # docker モードの postgres サービス名またはコンテナ名（既定は postgres）
EXEC_KIND=""       # compose | docker（docker モードで使う exec。空なら自動）
NEWPASS=""
ASSUME_YES=0

die() { echo "エラー: $*" >&2; exit 1; }
info() { echo "· $*" >&2; }

usage() { sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'; exit 0; }

# ---- 引数の解析 -------------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    -m|--mode)        MODE="${2:-}"; shift 2 ;;
    --dsn)            DSN="${2:-}"; shift 2 ;;
    -H|--host)        HOST="${2:-}"; shift 2 ;;
    -P|--port)        PORT="${2:-}"; shift 2 ;;
    -U|--user)        USER="${2:-}"; shift 2 ;;
    -W|--db-password) DBPASS="${2:-}"; shift 2 ;;
    -d|--dbname)      DBNAME="${2:-}"; shift 2 ;;
    --sslmode)        SSLMODE="${2:-}"; shift 2 ;;
    --config)         CONFIG="${2:-}"; shift 2 ;;
    -c|--container)   CONTAINER="${2:-}"; shift 2 ;;
    --exec)           EXEC_KIND="${2:-}"; shift 2 ;;
    -p|--new-password) NEWPASS="${2:-}"; shift 2 ;;
    -y|--yes)         ASSUME_YES=1; shift ;;
    -h|--help)        usage ;;
    *) die "不明な引数です: $1（使い方は -h）" ;;
  esac
done

# ---- config.json の database.* を読む（local モードで、接続を明示していないときだけ）-----
# python3 での解析を優先する（堅い）。python3 が無いときは grep に戻る（config.json はフィールドごとに整っている）。
read_config_json() {
  local path="$1"
  [[ -f "$path" ]] || return 1
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$path" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1])).get("database", {})
except Exception:
    sys.exit(1)
# dsn を直接渡すか、フィールドごとに渡す
if d.get("dsn"):
    print("DSN\t" + d["dsn"]); sys.exit(0)
for k in ("host","port","user","password","dbname","sslmode"):
    if d.get(k) is not None:
        print(k.upper() + "\t" + str(d[k]))
PY
  else
    # 最小限の予備。キーごとに grep する（値は文字列か数値）
    local k
    for k in host port user password dbname sslmode; do
      local v
      v=$(grep -oE "\"$k\"[[:space:]]*:[[:space:]]*(\"[^\"]*\"|[0-9]+)" "$path" 2>/dev/null \
            | head -1 | sed -E "s/.*:[[:space:]]*//; s/^\"//; s/\"$//") || true
      [[ -n "$v" ]] && echo -e "${k^^}\t$v"
    done
  fi
}

apply_config_fields() {
  local line key val
  while IFS=$'\t' read -r key val; do
    [[ -z "$key" ]] && continue
    case "$key" in
      DSN)      [[ -z "$DSN" ]] && DSN="$val" ;;
      HOST)     [[ -z "$HOST" ]] && HOST="$val" ;;
      PORT)     [[ -z "$PORT" ]] && PORT="$val" ;;
      USER)     [[ -z "$USER" ]] && USER="$val" ;;
      PASSWORD) [[ -z "$DBPASS" ]] && DBPASS="$val" ;;
      DBNAME)   [[ -z "$DBNAME" ]] && DBNAME="$val" ;;
      SSLMODE)  [[ -z "$SSLMODE" ]] && SSLMODE="$val" ;;
    esac
  done
}

# ---- モードの自動判定 ---------------------------------------------------------
if [[ -z "$MODE" ]]; then
  if [[ -n "$DSN$HOST$USER$DBNAME" || -n "${ARTEX_PG_DSN:-}" || -f "${CONFIG:-config.json}" ]]; then
    MODE="local"
  elif command -v docker >/dev/null 2>&1 && [[ -f docker-compose.yml ]]; then
    MODE="docker"
  else
    MODE="local"
  fi
fi
info "デプロイ方式: $MODE"

# ---- 新しいパスワードの入力 -----------------------------------------------------------
if [[ -z "$NEWPASS" ]]; then
  read -r -s -p "新しいパスワードを入力してください（ユーザー名は ARTEX で固定です）: " NEWPASS; echo >&2
  [[ -n "$NEWPASS" ]] || die "パスワードは空にできません"
  read -r -s -p "確認のためもう一度入力してください: " NEWPASS2; echo >&2
  [[ "$NEWPASS" == "$NEWPASS2" ]] || die "2 回の入力が一致しません"
fi
[[ -n "$NEWPASS" ]] || die "パスワードは空にできません"

# 環境変数でパスワードを psql に渡す（\getenv が読む。argv や ps には出ない）
export ARTEX_RESET_NEWPASS="$NEWPASS"

# ライブラリ内で bcrypt を作り upsert する。パスワードは :'newpw' で自動エスケープする。
# CREATE EXTENSION は冪等である。データベースロールに拡張を作る権限が無いと、ここで失敗する
# （案内は下の run の失敗分岐を参照）。
SQL=$(cat <<SQL
\\set ON_ERROR_STOP on
\\getenv newpw ARTEX_RESET_NEWPASS
CREATE EXTENSION IF NOT EXISTS pgcrypto;
INSERT INTO settings(key, value)
VALUES ('$PASS_KEY', crypt(:'newpw', gen_salt('bf', $BCRYPT_COST)))
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();
SQL
)

# ---- 実行 -----------------------------------------------------------------
if [[ "$MODE" == "local" ]]; then
  # 接続情報の優先順位: コマンド引数 > --dsn/$ARTEX_PG_DSN > config.json
  if [[ -z "$DSN" && -z "$HOST$USER$DBNAME" ]]; then
    [[ -n "${ARTEX_PG_DSN:-}" ]] && DSN="$ARTEX_PG_DSN"
  fi
  if [[ -z "$DSN" && -z "$HOST$USER$DBNAME" ]]; then
    cfg="${CONFIG:-config.json}"
    if [[ -f "$cfg" ]]; then
      info "データベース設定を $cfg から読みました"
      apply_config_fields < <(read_config_json "$cfg")
    fi
  fi

  command -v psql >/dev/null 2>&1 || die "このマシンに psql がありません。postgresql-client を入れるか、-m docker を使ってください"

  declare -a PSQL_ARGS=()
  if [[ -n "$DSN" ]]; then
    PSQL_ARGS=("$DSN")
    target="$DSN"
  else
    [[ -n "$USER"   ]] || die "データベースのユーザーがありません（-U、または有効な config.json / DSN）"
    [[ -n "$DBNAME" ]] || die "データベース名がありません（-d、または有効な config.json / DSN）"
    HOST="${HOST:-127.0.0.1}"; PORT="${PORT:-5432}"; SSLMODE="${SSLMODE:-disable}"
    PSQL_ARGS=(-h "$HOST" -p "$PORT" -U "$USER" -d "$DBNAME")
    [[ -n "$SSLMODE" ]] && export PGSSLMODE="$SSLMODE"
    [[ -n "$DBPASS" ]] && export PGPASSWORD="$DBPASS"
    target="$USER@$HOST:$PORT/$DBNAME"
  fi

  info "対象データベース: $target"
  if [[ "$ASSUME_YES" -ne 1 ]]; then
    read -r -p "このデータベースで ARTEX のパスワードをリセットしますか？ [y/N] " ans
    [[ "$ans" == "y" || "$ans" == "Y" ]] || die "中止しました"
  fi

  if ! printf '%s\n' "$SQL" | psql "${PSQL_ARGS[@]}" -v ON_ERROR_STOP=1 -q >/dev/null; then
    die "書き込みに失敗しました。pgcrypto の権限がない、または拡張が無い場合は、拡張を作成できるロールを使うか、先に CREATE EXTENSION pgcrypto を実行してください。"
  fi

else
  # ---- docker ----
  command -v docker >/dev/null 2>&1 || die "docker が見つかりません"
  CONTAINER="${CONTAINER:-postgres}"

  # exec の選び方: docker compose exec（サービス名）を優先し、なければ docker exec（コンテナ名）
  if [[ -z "$EXEC_KIND" ]]; then
    if docker compose version >/dev/null 2>&1 && [[ -f docker-compose.yml ]]; then
      EXEC_KIND="compose"
    else
      EXEC_KIND="docker"
    fi
  fi

  # コンテナ内 psql の資格情報: コマンド引数を優先し、次に .env の POSTGRES_*、その次に compose の既定（artex）
  if [[ -f .env ]]; then
    # shellcheck disable=SC1091
    set -a; . ./.env; set +a
  fi
  DUSER="${USER:-${POSTGRES_USER:-artex}}"
  DNAME="${DBNAME:-${POSTGRES_DB:-artex}}"
  [[ -n "$DBPASS" ]] && export PGPASSWORD="$DBPASS"
  [[ -z "${PGPASSWORD:-}" && -n "${POSTGRES_PASSWORD:-}" ]] && export PGPASSWORD="$POSTGRES_PASSWORD"

  info "対象: コンテナ $CONTAINER 内の psql -U $DUSER -d $DNAME（exec=$EXEC_KIND）"
  if [[ "$ASSUME_YES" -ne 1 ]]; then
    read -r -p "このコンテナのデータベースで ARTEX のパスワードをリセットしますか？ [y/N] " ans
    [[ "$ans" == "y" || "$ans" == "Y" ]] || die "中止しました"
  fi

  # -e は名前だけで値を付けない。現在の環境から継承し、パスワードは docker コマンドの argv に出ない。
  declare -a EXEC_CMD
  if [[ "$EXEC_KIND" == "compose" ]]; then
    EXEC_CMD=(docker compose exec -T -e ARTEX_RESET_NEWPASS -e PGPASSWORD "$CONTAINER"
              psql -U "$DUSER" -d "$DNAME" -v ON_ERROR_STOP=1 -q)
  else
    EXEC_CMD=(docker exec -i -e ARTEX_RESET_NEWPASS -e PGPASSWORD "$CONTAINER"
              psql -U "$DUSER" -d "$DNAME" -v ON_ERROR_STOP=1 -q)
  fi

  if ! printf '%s\n' "$SQL" | "${EXEC_CMD[@]}" >/dev/null; then
    die "書き込みに失敗しました。コンテナ名（-c）、データベースアカウント（.env の POSTGRES_*）、ロールの pgcrypto 権限を確認してください。"
  fi
fi

unset ARTEX_RESET_NEWPASS
echo "✓ ARTEX 管理者パスワードをリセットしました。ユーザー名 ARTEX と新しいパスワードでログインしてください（サービスの再起動は不要です）。"
