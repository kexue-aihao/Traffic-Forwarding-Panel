package contract

type Exit struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	GroupID   string `json:"group_id"`
	NodeID    string `json:"node_id"`
	Transport string `json:"transport"`
	Tunnel    Tunnel `json:"tunnel"`
	Weight    int    `json:"weight"`
	Enabled   bool   `json:"enabled"`
	Version   int64  `json:"version"`
	Online    bool   `json:"online"`
}
