package contract

const (
	GroupMonitor   = "monitor"
	GroupEntry     = "entry"
	GroupExit      = "exit"
	GroupChainExit = "chain_exit"
)

// An empty type preserves the behavior of groups created before types existed.
// New UI groups always choose one of the four explicit roles.
func (g Group) CanEnter() bool    { return g.Type == "" || g.Type == GroupEntry }
func (g Group) CanHostExit() bool { return g.Type == "" || g.Type == GroupExit }
func (g Group) CanExit() bool     { return g.CanHostExit() || g.Type == GroupChainExit }
func (g Group) CanEnroll() bool   { return g.Type != GroupChainExit }
