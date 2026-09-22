package platform

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

// 一条诊断最多等两分钟。它是「现在看一眼」的工具：攒着的旧请求既没有意义，
// 也会让节点重连后突然跑一批过期命令。
const lookingGlassTTL = 2 * time.Minute

// createLookingGlass 在指定节点上排一次网络诊断。
//
// 权限与节点运维同级（管理员 + Cookie 会话），并要求节点声明 looking-glass-v1：
// 老 Agent 收不到这类请求，与其让操作方等到超时，不如在创建时就说清楚。
//
// 参数在这里就被 contract.ParseLookingGlass 校验并规范化 —— 落到库里的是
// 规范化之后的主机/端口，agent 拿到的是可以直接当 argv 用的东西。
func (s *Server) createLookingGlass(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Method string `json:"method"`
		Target string `json:"target"`
	}
	if !decode(w, r, &in) {
		return
	}
	host, port, e := contract.ParseLookingGlass(in.Method, in.Target)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	target := host
	if port != "" {
		target = host + ":" + port
	}
	node := r.PathValue("id")
	actor, _ := UserFromContext(r.Context())

	var raw string
	if e := s.Store.DB.QueryRowContext(r.Context(), s.q("SELECT payload FROM cp_nodes WHERE id=?"), node).Scan(&raw); e != nil {
		fail(w, 404, "节点不存在")
		return
	}
	var n contract.Node
	if e := json.Unmarshal([]byte(raw), &n); e != nil {
		fail(w, 500, "节点数据不可用")
		return
	}
	if !contains(n.Capabilities, "looking-glass-v1") {
		fail(w, 409, "该节点未声明 looking-glass-v1，请先升级 Agent")
		return
	}
	// 同一个节点上同时只跑一条：这是一次同步的「看一眼」，排队没有意义，
	// 反而会让操作方对不上号。
	var pending int
	if e := s.Store.DB.QueryRowContext(r.Context(), s.q("SELECT COUNT(*) FROM cp_looking_glass WHERE node_id=? AND status IN ('pending','running') AND created_at>?"), node, time.Now().Add(-lookingGlassTTL).Unix()).Scan(&pending); e != nil {
		fail(w, 500, "诊断队列不可用")
		return
	}
	if pending > 0 {
		fail(w, 409, "该节点上已有一条诊断在执行")
		return
	}

	out := contract.LookingGlass{ID: id(), NodeID: node, Method: in.Method, Target: target, Status: "pending", CreatedAt: time.Now().UTC()}
	e = s.Store.Write(r.Context(), storage.Normal, func(tx *sql.Tx) error {
		if _, e := tx.ExecContext(r.Context(), s.q("DELETE FROM cp_looking_glass WHERE created_at<?"), time.Now().Add(-24*time.Hour).Unix()); e != nil {
			return e
		}
		_, e := tx.ExecContext(r.Context(), s.q("INSERT INTO cp_looking_glass(id,node_id,user_id,method,target,status,claim_token,payload,created_at,claimed_at,finished_at) VALUES(?,?,?,?,?,'pending','',?,?,0,0)"), out.ID, node, actor.ID, out.Method, out.Target, strJSON(out), out.CreatedAt.Unix())
		if e != nil {
			return e
		}
		return s.AuditTx(r.Context(), tx, actor.ID, "looking-glass", node)
	})
	if e != nil {
		fail(w, 500, "诊断队列不可用")
		return
	}
	reply(w, 202, out)
}

// lookingGlass 返回一条诊断的当前状态与输出。创建者本人或管理员可见。
func (s *Server) lookingGlass(w http.ResponseWriter, r *http.Request) {
	actor, _ := UserFromContext(r.Context())
	var raw, owner, status string
	var created int64
	if e := s.Store.DB.QueryRowContext(r.Context(), s.q("SELECT payload,user_id,status,created_at FROM cp_looking_glass WHERE id=?"), r.PathValue("id")).Scan(&raw, &owner, &status, &created); e != nil {
		fail(w, 404, "诊断不存在")
		return
	}
	if actor.Role != "admin" && actor.ID != owner {
		fail(w, 404, "诊断不存在")
		return
	}
	var out contract.LookingGlass
	if json.Unmarshal([]byte(raw), &out) != nil {
		fail(w, 500, "诊断数据不可用")
		return
	}
	out.Status = status
	out.Claim = ""
	if (status == "pending" || status == "running") && time.Now().Unix()-created > int64(lookingGlassTTL.Seconds()) {
		out.Status = "expired"
	}
	reply(w, 200, out)
}

// claimLookingGlass 由 Agent 调用，领走本节点上最早的一条待执行诊断。
//
// 认领带一个一次性令牌与 30 秒的抢占窗口：Agent 崩了之后，同一条请求会被
// 下一条轮询重新领走，而不是永远卡在 running。这与诊断（diagnostics）同构。
func (s *Server) claimLookingGlass(w http.ResponseWriter, r *http.Request) {
	node := r.Context().Value(nodeKey{}).(string)
	var out *contract.LookingGlass
	e := s.Store.Write(r.Context(), storage.Normal, func(tx *sql.Tx) error {
		var rid, raw string
		q := `SELECT g.id,g.payload FROM cp_looking_glass g JOIN cp_nodes n ON n.id=g.node_id WHERE g.node_id=? AND g.created_at>? AND (g.status='pending' OR (g.status='running' AND g.claimed_at<?)) ORDER BY g.created_at,g.id LIMIT 1`
		e := tx.QueryRowContext(r.Context(), s.q(q), node, time.Now().Add(-lookingGlassTTL).Unix(), time.Now().Add(-30*time.Second).Unix()).Scan(&rid, &raw)
		if errors.Is(e, sql.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		var v contract.LookingGlass
		if e := json.Unmarshal([]byte(raw), &v); e != nil {
			return e
		}
		v.Claim, v.Status = token(), "running"
		res, e := tx.ExecContext(r.Context(), s.q("UPDATE cp_looking_glass SET status='running',claim_token=?,claimed_at=? WHERE id=? AND (status='pending' OR (status='running' AND claimed_at<?))"), v.Claim, time.Now().Unix(), rid, time.Now().Add(-30*time.Second).Unix())
		if e != nil {
			return e
		}
		if n, _ := res.RowsAffected(); n == 1 {
			out = &v
		}
		return nil
	})
	if e != nil {
		fail(w, 500, "诊断队列不可用")
		return
	}
	reply(w, 200, map[string]any{"request": out})
}

// finishLookingGlass 由 Agent 回传结果。认领令牌必须对上，且只能写一次 ——
// 一条诊断的结果不该被后续请求覆盖。
func (s *Server) finishLookingGlass(w http.ResponseWriter, r *http.Request) {
	node := r.Context().Value(nodeKey{}).(string)
	var in contract.LookingGlass
	if !decode(w, r, &in) {
		return
	}
	if len(in.Output) > 64<<10 || len(in.Error) > 500 || in.Claim == "" {
		fail(w, 400, "invalid result")
		return
	}
	status := "done"
	if in.Error != "" {
		status = "failed"
	}
	e := s.Store.Write(r.Context(), storage.Normal, func(tx *sql.Tx) error {
		var raw string
		if e := tx.QueryRowContext(r.Context(), s.q("SELECT payload FROM cp_looking_glass WHERE id=? AND node_id=? AND status='running' AND claim_token=?"), in.ID, node, in.Claim).Scan(&raw); e != nil {
			return e
		}
		var v contract.LookingGlass
		if e := json.Unmarshal([]byte(raw), &v); e != nil {
			return e
		}
		v.Output, v.Error, v.Claim = in.Output, in.Error, ""
		res, e := tx.ExecContext(r.Context(), s.q("UPDATE cp_looking_glass SET status=?,payload=?,finished_at=? WHERE id=? AND status='running' AND claim_token=?"), status, strJSON(v), time.Now().Unix(), in.ID, in.Claim)
		if e != nil {
			return e
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("result already recorded")
		}
		return nil
	})
	if e != nil {
		fail(w, 409, "diagnostic result rejected")
		return
	}
	w.WriteHeader(204)
}
