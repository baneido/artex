package db

import (
	"encoding/json"
	"time"
)

// LLM の再試行方針。5 層の再試行の「回数と間隔」の全体設定。docs/LLM重试设计.md を参照。
// settings 表の JSON 値 1 つに置く。マシン全体で 1 組の実行パラメータであり、これのために表は開かない。
// 読み取りは組み込み既定で補う。キーが無い（新しい庫、または一度も設定していない）ときは、定数を直書きしていた頃と振る舞いが一致する。

const settingLLMRetryPolicy = "llm_retry_policy"

// RetryRule is one layer's knob pair. The zero value means "unset":
//
//	Attempts   0 は組み込みの既定回数を使う。-1 はその層の再試行を閉じる。0 より大きければその値を使う
//	IntervalMS 0 はその層の元の間隔方針を使う（通常は指数バックオフ）。0 より大きければ固定のミリ秒間隔にする
//
// -1 は「明示的に閉じる」であり、「0 回」ではない。0 はすでに「未設定」が使っている。
type RetryRule struct {
	Attempts   int `json:"attempts"`
	IntervalMS int `json:"interval_ms"`
}

// Interval returns the configured fixed interval, or 0 when unset (caller keeps
// its own default ladder).
func (r RetryRule) Interval() time.Duration {
	if r.IntervalMS <= 0 {
		return 0
	}
	return time.Duration(r.IntervalMS) * time.Millisecond
}

// Or returns the rule with each unset field filled in from fallback. Used to
// layer a profile override on top of the global policy field by field, so a
// profile that only pins the interval still inherits the global count.
func (r RetryRule) Or(fallback RetryRule) RetryRule {
	if r.Attempts == 0 {
		r.Attempts = fallback.Attempts
	}
	if r.IntervalMS == 0 {
		r.IntervalMS = fallback.IntervalMS
	}
	return r
}

// retry knob bounds. A count above the cap turns a blip into a token bonfire;
// an interval above an hour outlives any transient failure worth waiting out.
const (
	maxRetryAttempts   = 20
	maxRetryIntervalMS = 3600_000 // 1h
)

// Clamped returns the rule with out-of-range values pulled back into the sane
// band (attempts within [-1, 20], interval within [0, 1h]).
func (r RetryRule) Clamped() RetryRule {
	if r.Attempts < -1 {
		r.Attempts = -1
	}
	if r.Attempts > maxRetryAttempts {
		r.Attempts = maxRetryAttempts
	}
	if r.IntervalMS < 0 {
		r.IntervalMS = 0
	}
	if r.IntervalMS > maxRetryIntervalMS {
		r.IntervalMS = maxRetryIntervalMS
	}
	return r
}

// Clamped bounds a profile's override the same way the global policy is bounded,
// so a hand-crafted API payload can't land a value the CHECK constraint rejects.
func (o RetryOverride) Clamped() RetryOverride {
	o.Connect, o.Empty, o.Stream = o.Connect.Clamped(), o.Empty.Clamped(), o.Stream.Clamped()
	return o
}

// LLMRetryPolicy holds the 5 層の retry configuration. Connect/Empty/Stream are the
// per-request layers (a profile may override them, see LLMProfile.Retry);
// Breaker and Intent are process-wide by nature and live only here.
type LLMRetryPolicy struct {
	// Connect。SDK の接続再試行（接続リセット、タイムアウト、429、5xx。ストリーム開始前）。既定は 3 回、指数バックオフ。
	Connect RetryRule `json:"connect"`
	// Empty。SDK の空応答の再試行（完了したが content block が無い。openai 形式だけ）。既定は 2 回、指数バックオフ。
	Empty RetryRule `json:"empty"`
	// Stream。同じ provider の安全窓の再試行（出力を渡す前の、切れたストリームの再送）。既定は 2 回、0.5 秒から指数（上限 4 秒）。
	Stream RetryRule `json:"stream"`
	// Breaker。ポーリングのサーキットブレーカ。Attempts は、連続した瞬間失敗がこの回数で遮断する（既定 3。-1 は瞬間失敗では遮断しない。
	// 残高不足や鍵の失効のような硬い失敗は、すぐに遮断する）。IntervalMS は固定の冷却時間（0 は既定の 1/5/30 分の段階）。
	Breaker RetryRule `json:"breaker"`
	// Intent。worker が model_error で終わったあとの、インテント全体の再実行。既定は 2 回、固定 3 秒。
	Intent RetryRule `json:"intent"`
}

// Clamped returns the policy with every rule clamped.
func (p LLMRetryPolicy) Clamped() LLMRetryPolicy {
	p.Connect, p.Empty, p.Stream = p.Connect.Clamped(), p.Empty.Clamped(), p.Stream.Clamped()
	p.Breaker, p.Intent = p.Breaker.Clamped(), p.Intent.Clamped()
	return p
}

// LLMRetryPolicy reads the global retry policy. A missing or unparseable value
// yields the zero policy — i.e. every layer on its built-in default.
func (d *DB) LLMRetryPolicy() LLMRetryPolicy {
	var p LLMRetryPolicy
	if d == nil {
		return p
	}
	raw, ok, err := d.GetSetting(settingLLMRetryPolicy)
	if err != nil || !ok || raw == "" {
		return p
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return LLMRetryPolicy{}
	}
	return p.Clamped()
}

// SetLLMRetryPolicy persists the global retry policy (values are clamped first).
func (d *DB) SetLLMRetryPolicy(p LLMRetryPolicy) error {
	raw, err := json.Marshal(p.Clamped())
	if err != nil {
		return err
	}
	return d.SetSetting(settingLLMRetryPolicy, string(raw))
}
