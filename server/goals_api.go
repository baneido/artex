package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// 概要の「目標管理」の手動 CRUD。agent 側の set_goals と同じ goal ノードへ書く。
// 入口は人が UI で直接追加、変更、削除することである。追加と変更のあとは「タスクを復帰させる」処理を使う（admitTask の resume。
// 終了状態を running にし、一時停止を解除し、必要ならキューに入れる）。削除は復帰させない（製品上の決定）。各変更 handler は
// beginTaskOperation/decInflight を通り、タスク削除との競合を避ける（インテントの CRUD と同じ）。

// listGoals はこのタスクの目標をすべて返す（text/vulnclass/state を分解済み）。目標管理カードの描画に使う。
func (s *Server) listGoals(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	goals, err := t.Store.ListByKind(db.KindGoal, 10000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"goals": goalDTOs(goals)})
}

// addGoal は目標を 1 件手で追加する。保存し（タスクのルートの spawns の下）、 「目標を追加した」トリガーで
// planner を起こし、タスクを復帰させ、プランナーに新しい目標で達成を判定し直させる。
func (s *Server) addGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "タスクを削除中のため、目標を追加できません")
		return
	}
	defer s.engine.decInflight(t.ID)

	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "目標の内容は空にできません")
		return
	}
	payload := map[string]any{"text": text}
	if vc := strings.TrimSpace(body.VulnClass); vc != "" {
		payload["vulnclass"] = vc
	}
	id, err := t.Store.AddGoal(payload, "human")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if of, _ := t.Store.OriginFactID(); of > 0 && id > 0 {
		_ = t.Store.Link(of, db.RelSpawns, id) // goal descends from the task root (origin fact)
	}
	t.NotifyGoal([]string{text}) // 「人が目標を追加した: …」トリガーを記録し、planner を起こす
	s.reviveTask(t)              // 完了または一時停止のタスクを実行状態に戻して続ける
	node, _ := t.Store.GetNode(id)
	if node == nil {
		writeErr(w, 500, "目標の書き込み後の読み取りに失敗しました")
		return
	}
	writeJSON(w, 200, goalDTO(node))
}

// editGoal は目標のテキスト（と vulnclass）を手で変える。保存し、「ユーザーが目標を old から new に変えた」
// トリガーで planner を起こし、タスクを復帰させ、プランナーに新しい目標で方向を調整させる。
func (s *Server) editGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "タスクを削除中のため、目標を変更できません")
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "目標の内容は空にできません")
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, "目標が存在しません")
		return
	}
	oldText := goalDTO(node).Text
	if err := t.Store.UpdateGoalPayload(gid, text, strings.TrimSpace(body.VulnClass)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalEdited(oldText, text) // 「人が目標を old から new に変えた」トリガーを記録し、planner を起こす
	s.reviveTask(t)                   // 追加と同じ。タスクを復帰させ、新しい目標で判定し直す
	updated, _ := t.Store.GetNode(gid)
	if updated == nil {
		writeErr(w, 500, "目標の更新後の読み取りに失敗しました")
		return
	}
	writeJSON(w, 200, goalDTO(updated))
}

// deleteGoal は目標を 1 件手で削除する（物理削除。辺とアンカーも連鎖して消す）。保存から消し、「ユーザーが目標 X を削除した」
// トリガーで planner を起こし、残りの目標を判定し直させる。製品上の決定により、削除はタスクを復帰させない。
func (s *Server) deleteGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "タスクを削除中のため、目標を削除できません")
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, "目標が存在しません")
		return
	}
	text := goalDTO(node).Text
	if err := t.Store.DeleteGoal(gid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalDeleted(text) // 「人がこの目標を削除した: …」トリガーを記録し、planner を起こす（タスクは復帰させない）
	writeJSON(w, 200, map[string]bool{"ok": true})
}
