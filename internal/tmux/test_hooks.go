package tmux

type clientTestHooks struct {
	afterSpawn         func(string) error
	afterSpawnCommand  func(string) error
	afterMarkers       func(string) error
	spawnArgs          func(string, []string) []string
	spawnRunResult     func(string, string, error) (string, error)
	beforeMarkerWrite  func(string) error
	beforeIdentityKill func()
	beforeServerKill   func()
}

// SetSpawnFailureForTest injects an error after tmux has physically created
// or respawned a pane. It exercises the partial-spawn rollback contract.
func (c *Client) SetSpawnFailureForTest(inject func(string) error) {
	if c.testHooks == nil {
		c.testHooks = &clientTestHooks{}
	}
	c.testHooks.afterSpawn = inject
}

// SetFinalSpawnFailureForTest injects an error after both identity options are
// durable but before CreateSession's final observation. It exercises the
// boundary where another backend can already converge the candidate.
func (c *Client) SetFinalSpawnFailureForTest(inject func(string) error) {
	if c.testHooks == nil {
		c.testHooks = &clientTestHooks{}
	}
	c.testHooks.afterMarkers = inject
}

// WriteObservation is test evidence emitted immediately before a terminal
// write attempt crosses into tmux. Production never installs an observer; the
// acceptance gate uses it to count exactly which keystrokes invoke the
// physical writer, including attempts whose acknowledgement is lost or times
// out.
type WriteObservation struct {
	PaneID string
	Data   []byte
	Key    string
}

// SetWriteObserverForTest installs the acceptance suite's write-boundary
// observer. Passing nil removes it. The callback runs synchronously before a
// physical write attempt and must return quickly.
func (c *Client) SetWriteObserverForTest(observer func(WriteObservation)) {
	c.writeObserverMu.Lock()
	c.writeObserver = observer
	c.writeObserverMu.Unlock()
}

func (c *Client) observeWrite(observation WriteObservation) {
	c.writeObserverMu.RLock()
	observer := c.writeObserver
	c.writeObserverMu.RUnlock()
	if observer != nil {
		observer(observation)
	}
}

// AttachObservation describes one physical tmux client attachment.
type AttachObservation struct {
	SessionName string
	Cols        uint16
	Rows        uint16
}

// SetAttachObserverForTest installs a test-only observation at the physical
// tmux attach boundary. Production never installs one.
func (c *Client) SetAttachObserverForTest(observer func(AttachObservation)) {
	c.attachObserverMu.Lock()
	c.attachObserver = observer
	c.attachObserverMu.Unlock()
}

func (c *Client) observeAttach(observation AttachObservation) {
	c.attachObserverMu.RLock()
	observer := c.attachObserver
	c.attachObserverMu.RUnlock()
	if observer != nil {
		observer(observation)
	}
}
