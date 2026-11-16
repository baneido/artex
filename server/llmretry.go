package server

import (
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// 再試行方針のサーバー側の解釈。docs/LLM重试设计.md。5 層のうち、
//   - 接続、空応答、同じ provider の安全な窓は「エンドポイントに従う」。各 LLM 設定が上書きできる。
//     全体のデフォルト（profile の項目が空なら全体を継承し、全体も無ければ組み込みデフォルト）。
//   - サーキットブレーカーとインテントの再実行はプロセス単位で、全体に 1 つだけある。
//
// 全体方針は settings を 1 行読む。呼び出しは低頻度（provider の構築、work の終了処理、
// 設定の保存）なので、もう 1 段キャッシュする価値は無い。サーキットブレーカーのパラメータは例外で、失敗の経路で毎回読む。そのため
// applyRetryPolicy が Registry へ渡して保存する。

// retryPolicy reads the global policy; a nil DB yields the zero policy (all
// layers on their built-in defaults).
func (s *Server) retryPolicy() db.LLMRetryPolicy {
	if s.m == nil || s.m.pg == nil {
		return db.LLMRetryPolicy{}
	}
	return s.m.pg.LLMRetryPolicy()
}

// resolveRetry layers one profile's override on top of the global policy and
// converts the result into the form agent.Config carries. Rules combine field by
// field, so a profile that only pins an interval still inherits the global count.
func resolveRetry(o db.RetryOverride, pol db.LLMRetryPolicy) agent.RetryConfig {
	connect := o.Connect.Or(pol.Connect)
	empty := o.Empty.Or(pol.Empty)
	stream := o.Stream.Or(pol.Stream)
	return agent.RetryConfig{
		// 回数はここでは「0 はデフォルト、負はオフ」という元の意味のままにする。SDK の MaxRetries と
		// EmptyResponseRetries は同じ形なので、解釈はそちらに任せる。
		ConnectAttempts: connect.Attempts, ConnectInterval: connect.Interval(),
		EmptyAttempts: empty.Attempts, EmptyInterval: empty.Interval(),
		StreamAttempts: stream.Attempts, StreamInterval: stream.Interval(),
	}
}

// applyProfileRetry fills cfg.Retry for a profile read from the DB.
func (s *Server) applyProfileRetry(cfg *agent.Config, p *db.LLMProfile) {
	if p == nil {
		return
	}
	cfg.Retry = resolveRetry(p.Retry, s.retryPolicy())
}

// サーキットブレーカー（フェイルオーバーの冷却）のデフォルトは、llmpool の組み込みと同じである。ユーザーが値を入れたときだけ上書きする。
// インテント再実行のデフォルトは engine.go の modelErrorRetries と modelErrorRetryBackoff。

// applyRetryPolicy pushes the process-wide layers of the policy into the objects
// that consume them on a hot path: the circuit-breaker registry. Called at
// startup and whenever the policy is saved.
func (s *Server) applyRetryPolicy() {
	pol := s.retryPolicy()
	if s.llmHealth != nil {
		s.llmHealth.SetPolicy(pol.Breaker.Attempts, pol.Breaker.Interval())
	}
}

// modelErrorRetryPolicy resolves the intent-level replay knobs (layer ⑤): how
// many times a model_error work is re-run and how long to back off between runs.
func (e *Engine) modelErrorRetryPolicy() (retries int, backoff time.Duration) {
	retries, backoff = modelErrorRetries, modelErrorRetryBackoff
	if e == nil || e.m == nil || e.m.pg == nil {
		return retries, backoff
	}
	rule := e.m.pg.LLMRetryPolicy().Intent
	if rule.Attempts != 0 {
		retries = max(rule.Attempts, 0)
	}
	if d := rule.Interval(); d > 0 {
		backoff = d
	}
	return retries, backoff
}

// emptyTurnNudgeLimit resolves how many empty-turn continuations one work may
// inject (see steerHooks.Stop). It deliberately reuses layer ②'s knob. 「空応答の
// 再試行回数」。どちらも同じことの手段が違う。SDK の層は「内容ブロックが 1 つも無い」を扱い、手段は
// 同じリクエストをそのまま再送することである。ここは「思考だけで、本文もツールも無い」を扱い、手段は指示を 1 つ足して
// モデルに、すでにある思考の続きをさせることである（文脈の形で起きる空転に、そのままの再送は意味が無い）。空の判定が
// 違うのは、SDK が「イベントを yield したか」を見、思考の増分自体がイベントだからである。ユーザーが
// 「空応答を何回再試行するか」に入れる意味は「モデルが実質的な内容を出さなければもう一度」であり、2 層が回数を共有して
// その理解と合う。
//
// ある profile の上書きではなく全体方針を読む。1 つの run の途中でフェイルオーバーが profile を替えることがあり、これは
// インテント全体の総量の門なので、エンドポイントが替わっても変えない。意味は SDK の emptyRetries() と同じ形である。
// 0 はデフォルトの defaultEmptyTurnNudges。-1（負）は空転の継続をオフ。0 より大きければその値を使う。
func (e *Engine) emptyTurnNudgeLimit() int {
	if e == nil || e.m == nil || e.m.pg == nil {
		return defaultEmptyTurnNudges
	}
	switch n := e.m.pg.LLMRetryPolicy().Empty.Attempts; {
	case n == 0:
		return defaultEmptyTurnNudges
	case n < 0:
		return 0
	default:
		return n
	}
}
