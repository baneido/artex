package db

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// 資産インターセプトルールの照合と実行。asset_intercept.go はルールの保存だけを担う。ここでは
// 「対象資産」のドメイン、IP、URL を、有効なルールと照合する。agent ツール（add_intent、
// insert_assets）がインテントを出す前、資産を入れる前に呼び、一致したら拒否する。

// AssetInterceptKindLabel は kind の日本語ラベルを返す。agent への説明に使う。
func AssetInterceptKindLabel(kind string) string {
	switch kind {
	case "exact_domain":
		return "ドメイン（完全一致）"
	case "exact_ip":
		return "IP（完全一致）"
	case "exact_url":
		return "URL（完全一致）"
	case "fuzzy_domain":
		return "ドメイン（あいまい一致）"
	case "fuzzy_ip":
		return "IP（あいまい一致）"
	case "fuzzy_url":
		return "URL（あいまい一致）"
	case "cidr":
		return "CIDR"
	}
	return kind
}

// Reason は読める一致理由を返す。例: 資産インターセプトルールに一致しました [ドメイン（あいまい一致）: .gov.cn]（備考）。
func (r AssetInterceptRule) Reason() string {
	s := fmt.Sprintf("資産インターセプトルールに一致しました [%s: %s]", AssetInterceptKindLabel(r.Kind), r.Pattern)
	if note := strings.TrimSpace(r.Note); note != "" {
		s += "（" + note + "）"
	}
	return s
}

// matchOne は、有効なルール 1 件が、渡されたドメイン、IP、URL の候補に一致するかを見て、一致した値を返す。
func matchOne(r AssetInterceptRule, domains, ips, urls []string) (string, bool) {
	p := strings.TrimSpace(r.Pattern)
	if p == "" {
		return "", false
	}
	switch r.Kind {
	case "exact_domain":
		for _, d := range domains {
			if strings.EqualFold(strings.TrimSpace(d), p) {
				return d, true
			}
		}
	case "exact_ip":
		for _, ip := range ips {
			if strings.TrimSpace(ip) == p {
				return ip, true
			}
		}
	case "exact_url":
		for _, u := range urls {
			if strings.TrimSpace(u) == p {
				return u, true
			}
		}
	case "fuzzy_domain":
		lp := strings.ToLower(p)
		for _, d := range domains {
			if d != "" && strings.Contains(strings.ToLower(d), lp) {
				return d, true
			}
		}
	case "fuzzy_ip":
		for _, ip := range ips {
			if ip != "" && strings.Contains(ip, p) {
				return ip, true
			}
		}
	case "fuzzy_url":
		lp := strings.ToLower(p)
		for _, u := range urls {
			if u != "" && strings.Contains(strings.ToLower(u), lp) {
				return u, true
			}
		}
	case "cidr":
		_, ipnet, err := net.ParseCIDR(p)
		if err != nil {
			return "", false
		}
		for _, ip := range ips {
			if pip := net.ParseIP(strings.TrimSpace(ip)); pip != nil && ipnet.Contains(pip) {
				return ip, true
			}
		}
	}
	return "", false
}

// MatchAssetInterceptRules は、渡されたドメイン、IP、URL の候補に最初に一致した有効ルールと、一致した値を返す。
// insert_assets が、まだ保存していない元の入力（assetInputItem）を照合するのに使う。
func MatchAssetInterceptRules(rules []AssetInterceptRule, domains, ips, urls []string) (AssetInterceptRule, string, bool) {
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		if v, ok := matchOne(r, domains, ips, urls); ok {
			return r, v, true
		}
	}
	return AssetInterceptRule{}, "", false
}

// interceptCandidates は、保存済み資産からインターセプト照合用のドメイン、IP、URL の候補を取り出す。
// URL の host は分解して分類する。「URL だけ」のサービス資産も、ドメインや IP のルールに一致できるようにする。
func (a *Asset) interceptCandidates() (domains, ips, urls []string) {
	add := func(dst *[]string, s string) {
		if s = strings.TrimSpace(s); s != "" {
			*dst = append(*dst, s)
		}
	}
	add(&domains, a.Domain)
	add(&domains, a.RootDomain)
	for _, d := range a.BoundDomains {
		add(&domains, d)
	}
	add(&ips, a.IP)
	add(&urls, a.URL)
	if a.URL != "" {
		if u, err := url.Parse(a.URL); err == nil {
			if h := u.Hostname(); h != "" {
				if net.ParseIP(h) != nil {
					add(&ips, h)
				} else {
					add(&domains, h)
				}
			}
		}
	}
	return domains, ips, urls
}

// InterceptLabel は資産の短い識別子を返す。agent への説明に使う。
func (a *Asset) InterceptLabel() string {
	var target string
	switch {
	case a.Domain != "":
		target = a.Domain
	case a.URL != "":
		target = a.URL
	case a.IP != "":
		target = a.IP
	default:
		target = fmt.Sprintf("#%d", a.ID)
	}
	return fmt.Sprintf("資産#%d[%s] %s", a.ID, a.Type, target)
}

// hasEnabledRule は、ルール集合に有効なルールが 1 件でもあるかを見る。
func hasEnabledRule(rules []AssetInterceptRule) bool {
	for _, r := range rules {
		if r.Enabled {
			return true
		}
	}
	return false
}

// AssetGateDecision は、「先にインターセプト、その後に許可」のゲートが候補の組に下した判定である。
type AssetGateDecision struct {
	Allowed bool
	Reason  string // 拒否理由（資産の識別子は含まない）。Allowed=true のときは空
}

// EvaluateAssetGate はタスク単位のゲートを判定する。
//  1. 有効な blockRules のどれかに一致したら拒否する（インターセプトの理由）。
//  2. そうでなく、allowRules に有効な項目があり、どれにも一致しなければ拒否する（許可範囲の外）。
//  3. それ以外は通す。
//
// allowRules が空、または有効な項目が無いとき、許可ゲートは働かない（ホワイトリストを有効にしない。すべて通す）。
// 「許可ルールが未設定」で全資産を止めないためである。
func EvaluateAssetGate(blockRules, allowRules []AssetInterceptRule, domains, ips, urls []string) AssetGateDecision {
	if rule, _, ok := MatchAssetInterceptRules(blockRules, domains, ips, urls); ok {
		return AssetGateDecision{Allowed: false, Reason: rule.Reason()}
	}
	if hasEnabledRule(allowRules) {
		if _, _, ok := MatchAssetInterceptRules(allowRules, domains, ips, urls); !ok {
			return AssetGateDecision{Allowed: false, Reason: "タスクの許可（ホワイトリスト）範囲外のため、テストできません"}
		}
	}
	return AssetGateDecision{Allowed: true}
}

// AssetInterceptHit は、ゲートが拒否した資産を表す（インターセプトに一致したか、許可範囲の外か）。
type AssetInterceptHit struct {
	Asset  *Asset
	Reason string // 読める理由
}

// Describe は読める説明を返す。資産の情報と理由である。
func (h AssetInterceptHit) Describe() string {
	return fmt.Sprintf("%s → %s", h.Asset.InterceptLabel(), h.Reason)
}

// ListAssetInterceptRules は *DB の同名メソッドをそのまま渡す。AssetStore だけを持つ呼び出し側
// （agent ツールなど）もルールを読めるようにする。
func (s *AssetStore) ListAssetInterceptRules() ([]AssetInterceptRule, error) {
	return s.db.ListAssetInterceptRules()
}

// CheckAssetsIntercept は id で資産を読み、「先にインターセプト、その後に許可」のゲートを 1 件ずつ判定し、
// 拒否された資産をすべて返す。インターセプトルールは全体とタスク単位の block の和。許可ルールはタスク単位の allow（このタスクだけ）。
// id が無ければすぐ戻る。全体の GetByIDs（タスク範囲では絞り込まない）を使い、インターセプトが scope で弱まらないようにする。
func (s *AssetStore) CheckAssetsIntercept(taskID int64, ids []int64) ([]AssetInterceptHit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	blockRules, err := s.db.ListAssetInterceptRules()
	if err != nil {
		return nil, err
	}
	var allowRules []AssetInterceptRule
	if taskID > 0 {
		tb, ta, err := s.TaskInterceptRulesSplit(taskID)
		if err != nil {
			return nil, err
		}
		blockRules = append(blockRules, tb...)
		allowRules = ta
	}
	// インターセプトルールも、有効な許可ルールも無い。判定は不要で、すべて通す。
	if len(blockRules) == 0 && !hasEnabledRule(allowRules) {
		return nil, nil
	}
	assets, err := s.GetByIDs(ids)
	if err != nil {
		return nil, err
	}
	var hits []AssetInterceptHit
	for _, a := range assets {
		domains, ips, urls := a.interceptCandidates()
		if d := EvaluateAssetGate(blockRules, allowRules, domains, ips, urls); !d.Allowed {
			hits = append(hits, AssetInterceptHit{Asset: a, Reason: d.Reason})
		}
	}
	return hits, nil
}
