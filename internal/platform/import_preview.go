package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

type ImportRequest struct {
	Rules []contract.Rule `json:"rules"`
	Key   string          `json:"idempotency_key"`
	Mode  string          `json:"mode"`
}
type ImportPreviewItem struct {
	Index  int            `json:"index"`
	Action string         `json:"action"`
	Rule   *contract.Rule `json:"rule,omitempty"`
	Error  string         `json:"error,omitempty"`
}

func validImportMode(mode string) bool {
	return mode == "" || mode == "create" || mode == "update_by_port"
}

func (s *Server) importInputTx(ctx context.Context, tx *sql.Tx, owner string, admin bool, rule contract.Rule, mode string, preview bool) (contract.Rule, error) {
	if mode != "update_by_port" {
		return s.importRuleTx(ctx, tx, owner, admin, rule)
	}
	if !admin || rule.UserID == "" {
		rule.UserID = owner
	}
	// Owner lock also serializes updates and previews with normal rule creation.
	if err := s.lockRuleOwner(ctx, tx, rule.UserID); err != nil {
		return rule, err
	}
	var raw string
	_, port, err := splitListener(rule.Listen)
	if err != nil {
		return rule, err
	}
	err = tx.QueryRowContext(ctx, s.q("SELECT r.payload FROM cp_rules r JOIN cp_ports p ON p.rule_id=r.id WHERE p.node_id=? AND p.network=? AND p.port=? AND r.deleted=0 AND r.user_id=?"), rule.NodeID, rule.Network, port, rule.UserID).Scan(&raw)
	if err != nil {
		return rule, errors.New("no matching owned rule at this node/protocol/port")
	}
	var old contract.Rule
	if err = json.Unmarshal([]byte(raw), &old); err != nil {
		return rule, err
	}
	if preview {
		rule.Version = old.Version
	} else if rule.Version != old.Version {
		return rule, errors.New("preview is stale; refresh before updating")
	}
	rule.ID = old.ID
	actor := contract.User{ID: owner, Role: "user"}
	if admin {
		actor.Role = "admin"
	}
	err = s.saveRuleTx(ctx, tx, actor, &rule, false)
	return rule, err
}

var errPreviewRollback = errors.New("preview rollback")

func (s *Server) previewImport(w http.ResponseWriter, r *http.Request) {
	actor, _ := UserFromContext(r.Context())
	var in ImportRequest
	if !decode(w, r, &in) {
		return
	}
	if len(in.Rules) < 1 || len(in.Rules) > 1000 || !validImportMode(in.Mode) {
		fail(w, 400, "1-1000 rules and a valid import mode required")
		return
	}
	if !s.allow("import-preview:"+actor.ID, 6) {
		fail(w, 429, "preview rate limited")
		return
	}
	items := []ImportPreviewItem{}
	err := s.Store.Write(r.Context(), storage.Normal, func(tx *sql.Tx) error {
		for i, input := range in.Rules {
			if _, err := tx.ExecContext(r.Context(), "SAVEPOINT preview_item"); err != nil {
				return err
			}
			rule, e := s.importInputTx(r.Context(), tx, actor.ID, actor.Role == "admin", input, in.Mode, true)
			item := ImportPreviewItem{Index: i, Action: "create"}
			if in.Mode == "update_by_port" {
				item.Action = "update"
			}
			if e != nil {
				if _, err := tx.ExecContext(r.Context(), "ROLLBACK TO SAVEPOINT preview_item"); err != nil {
					return err
				}
				item.Error = e.Error()
			} else {
				redact(&rule)
				rule.Version--
				if item.Action == "create" {
					rule.ID = ""
				}
				item.Rule = &rule
			}
			if _, err := tx.ExecContext(r.Context(), "RELEASE SAVEPOINT preview_item"); err != nil {
				return err
			}
			items = append(items, item)
		}
		return errPreviewRollback
	})
	if !errors.Is(err, errPreviewRollback) {
		fail(w, 500, "preview unavailable")
		return
	}
	reply(w, 200, map[string]any{"items": items, "mode": in.Mode})
}

func splitListener(v string) (string, int, error) {
	h, p, e := net.SplitHostPort(v)
	if e != nil {
		return h, 0, e
	}
	n, e := strconv.Atoi(p)
	if e != nil || n < 1 || n > 65535 {
		return h, 0, errors.New("invalid listen port")
	}
	return h, n, nil
}
