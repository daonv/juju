package singular_test

import (
	"fmt"
	"time"
	"strings"

	"github.com/juju/loggo"
	"labix.org/v2/mgo"

	gc "launchpad.net/gocheck"
	"launchpad.net/juju-core/replicaset"
	"launchpad.net/juju-core/testing"
	"launchpad.net/juju-core/testing/testbase"
	"launchpad.net/juju-core/utils"
	"launchpad.net/juju-core/worker"
	"launchpad.net/juju-core/worker/singular"
)

var logger = loggo.GetLogger("juju.singular-test")

type mongoSuite struct {
	testbase.LoggingSuite
}

var _ = gc.Suite(&mongoSuite{})

// start replica set with three mongods
// start singular worker on each one.
// change worker priorities so the master changes.
// check that
// a) there is never more than one running at a time
// b) the running worker changes when the master changes.

func (*mongoSuite) TestMongoMastership(c *gc.C) {
	insts, err := startReplicaSet(3)
	c.Assert(err, gc.IsNil)
	for _, inst := range insts {
		defer inst.Destroy()
	}

	notifyCh := make(chan event, 100)
	expect := func(possible ...event) event {
		return expectNotification(c, notifyCh, possible...)
	}

	globalState := newGlobalAgentState(len(insts), expect)

	agents := startAgents(c, notifyCh, insts)

	assertAgentsConnect(c, globalState)

	// Wait for one of the agents to start.
	for globalState.activeId == -1 {
		globalState.waitEvent(c)
	}
	c.Logf("agent %d started; waiting for servers to sync", globalState.activeId)
	time.Sleep(1 * time.Minute)

	// Choose a different agent than the primary to
	// make master.
	nextId := ((globalState.activeId + 1) -1) % len(insts) + 1

	c.Logf("giving agent %d priority to become master", nextId)
	changeVotes(c, insts, nextId)

	// Wait for the first agent to stop and another agent
	// to start. Note that because of mongo's vagaries, we
	// cannot be sure which agent will actually start, even
	// though we've set our priorities to hope that a
	// particular mongo instance (nextId) becomes master.
	oldId := globalState.activeId
	oldHasStopped := false
	for {
		if oldHasStopped && globalState.activeId != -1 {
			break
		}
		got := globalState.waitEvent(c)
		if got.kind == "stop" && got.id == oldId {
			oldHasStopped = true
		}
	}

	// Kill all the agents and wait for them to quit.
	for _, a := range agents {
		if a.Runner == nil {
			panic("runner is nil")
		}
		a.Kill()
	}

	assertAgentsQuit(c, globalState)
}

func startAgents(c *gc.C, notifyCh chan<- event, insts []*testing.MgoInstance) []*agent {
	agents := make([]*agent, len(insts))
	for i, inst := range insts {
		a := &agent{
			// Note: we use ids starting from 1 to match
			// the replica set ids.
			notify: &notifier{
				id: i+1,
				ch:    notifyCh,
			},
			Runner:   newRunner(),
			hostPort: inst.Addr(),
		}
		go func() {
			err := a.run()
			a.notify.agentQuit(err)
		}()
		agents[i] = a
	}
	return agents
}

// assertAgentsConnect waits for all the agents to connect.
func assertAgentsConnect(c *gc.C, globalState *globalAgentState) {
	allConnected := func() bool {
		for _, connected := range globalState.connected {
			if !connected {
				return false
			}
		}
		return true
	}
	for !allConnected() {
		globalState.waitEvent(c)
	}
}

func assertAgentsQuit(c *gc.C, globalState *globalAgentState) {
	allQuit := func() bool {
		for _, quit := range globalState.quit {
			if !quit {
				return false
			}
		}
		return true
	}
	for !allQuit() {
		globalState.waitEvent(c)
	}
}

type agent struct {
	notify *notifier
	worker.Runner
	hostPort string
}

func (a *agent) run() error {
	a.Runner.StartWorker(fmt.Sprint("mongo-", a.notify.id), a.mongoWorker)
	return a.Runner.Wait()
}

func (a *agent) mongoWorker() (worker.Worker, error) {
	dialInfo := testing.MgoDialInfo(a.hostPort)
	session, err := mgo.DialWithInfo(dialInfo)
	if err != nil {
		return nil, err
	}
	mc := &mongoConn{
		localHostPort: a.hostPort,
		session:       session,
	}
	runner := worker.NewRunner(
		connectionIsFatal(mc),
		func(err0, err1 error) bool { return true },
	)
	singularRunner, err := singular.New(runner, mc)
	if err != nil {
		return nil, fmt.Errorf("cannot start singular runner: %v", err)
	}
	a.notify.workerConnected()
	singularRunner.StartWorker(fmt.Sprint("worker-", a.notify.id), func() (worker.Worker, error) {
		return worker.NewSimpleWorker(func(stop <-chan struct{}) error {
			return a.worker(session, stop)
		}), nil
	})
	return runner, nil
}

func (a *agent) worker(session *mgo.Session, stop <-chan struct{}) error {
	a.notify.workerStarted()
	defer a.notify.workerStopped()
	coll := session.DB("foo").C("bar")
	for {
		select {
		case <-stop:
			return nil
		case <-time.After(250 * time.Millisecond):
		}
		if err := coll.Insert(struct{}{}); err != nil {
			return fmt.Errorf("insert error: %v", err)
		}
		a.notify.operation()
	}
}

type globalAgentState struct {
	expect func(...event) event

	numAgents int
	connected []bool
	started []bool
	quit []bool
	activeId int
}

func newGlobalAgentState(n int, expect func(...event) event) *globalAgentState {
	return &globalAgentState{
		expect: expect,
		numAgents: n,
		connected: make([]bool, n),
		started: make([]bool, n),
		quit: make([]bool, n),
		activeId: -1,
	}
}

func boolsToStr(b []bool) string {
	d := make([]byte, len(b))
	for i, ok := range b {
		if ok {
			d[i] = '1'
		} else {
			d[i] = '0'
		}
	}
	return string(d)
}

func (g *globalAgentState) String() string {
	return fmt.Sprintf("{active %d; connected %s; started %s; quit %s}",
		g.activeId,
		boolsToStr(g.connected),
		boolsToStr(g.started),
		boolsToStr(g.quit),
	)
}

func (g *globalAgentState) waitEvent(c *gc.C) event {
	c.Logf("awaiting event; current state %s", g)

	possible := g.possibleEvents()
	c.Logf("possible: %q", possible)

	got := g.expect(possible...)
	index := got.id - 1
	switch got.kind {
	case "connect":
		g.connected[index] = true
	case "start":
		g.started[index] = true
	case "operation":
		if g.activeId != -1 && g.activeId != got.id {
			c.Fatalf("mixed operations from different agents")
		}
		g.activeId = got.id
	case "stop":
		g.activeId = -1
		g.started[index] = false
	case "quit":
		g.quit[index] = true
		c.Assert(got.info, gc.IsNil)
	default:
		c.Fatalf("unexpected event %q", got)
	}
	return got
}

func (g *globalAgentState) possibleEvents() []event {
	var possible []event
	for i := 0; i < g.numAgents; i++ {
		isConnected, isStarted, hasQuit := g.connected[i], g.started[i], g.quit[i]
		id := i+1
		addPossible := func(kind string) {
			possible = append(possible, event{kind: kind, id: id})
		}
		if isConnected {
			if isStarted {
				if g.activeId == -1 || id == g.activeId {
					addPossible("operation")
				}
				addPossible("stop")
			} else {
				addPossible("start")
				addPossible("connect")
				if !hasQuit {
					addPossible("quit")
				}
			}
		} else {
			addPossible("connect")
		}
	}
	return possible
}

func mkEvent(s string) event {
	var e event
	if n, _ := fmt.Sscanf(s, "%s %d", &e.kind, &e.id); n != 2 {
		panic("invalid event " + s)
	}
	return e
}

func mkEvents(ss ...string) []event {
	events := make([]event, len(ss))
	for i, s := range ss {
		events[i] = mkEvent(s)
	}
	return events
}

type event struct {
	kind string
	id int
	info interface{}
}

func (e event) String() string {
	if e.info != nil {
		return fmt.Sprintf("%s %d %v", e.kind, e.id, e.info)
	} else {
		return  fmt.Sprintf("%s %d", e.kind, e.id)
	}
}

func oneOf(possible ...string) string {
	return strings.Join(possible, "|")
}

func expectNotification(c *gc.C, notifyCh <-chan event, possible ...event) event {
	select {
	case e := <-notifyCh:
		c.Logf("received notification %q", e)
		for _, p := range possible {
			if e.kind == p.kind && e.id == p.id {
				return e
			}
		}
		c.Fatalf("event %q does not match any of %q", e, possible)
		return e
	case <-time.After(testing.LongWait):
		c.Fatalf("timed out waiting for %q", possible)
	}
	panic("unreachable")
}

func changeVotes(c *gc.C, insts []*testing.MgoInstance, voteId int) {
	c.Logf("changing voting id to %v", voteId)

	addrs := make([]string, len(insts))
	for i, inst := range insts {
		addrs[i] = inst.Addr()
	}
	dialInfo := testing.MgoDialInfo(addrs...)

	session, err := mgo.DialWithInfo(dialInfo)
	c.Assert(err, gc.IsNil)
	defer session.Close()

	members, err := replicaset.CurrentMembers(session)
	c.Assert(err, gc.IsNil)	
	c.Assert(members, gc.HasLen, len(insts))
	for i := range members {
		member := &members[i]
		if member.Id == voteId {
			member.Priority = nil
		} else {
			member.Priority = newFloat64(0.1)
		}
	}
	c.Logf("new member set: %#v", members)
	err = replicaset.Set(session, members)
	c.Assert(err, gc.IsNil)

	c.Logf("successfully changed replica set members")
}

type notifier struct {
	id int
	ch    chan<- event
}

func (n *notifier) sendEvent(kind string, info interface{}) {
	n.ch <- event{
		id: n.id,
		kind: kind,
		info: info,
	}
}

func (n *notifier) workerConnected() {
	n.sendEvent("connect", nil)
}

func (n *notifier) workerStarted() {
	n.sendEvent("start", nil)
}

func (n *notifier) workerStopped() {
	n.sendEvent("stop", nil)
}

func (n *notifier) operation() {
	n.sendEvent("operation", nil)
}

func (n *notifier) agentQuit(err error) {
	n.sendEvent("quit", err)
}

type mongoConn struct {
	localHostPort string
	session       *mgo.Session
}

func (c *mongoConn) Ping() error {
	return c.session.Ping()
}

func (c *mongoConn) IsMaster() (bool, error) {
	hostPort, err := replicaset.MasterHostPort(c.session)
	if err != nil {
		logger.Errorf("replicaset.MasterHostPort returned error: %v", err)
		return false, err
	}
	logger.Errorf("replicaset.MasterHostPort(%s) returned %s", c.localHostPort, hostPort)
	logger.Errorf("-> %s IsMaster: %v", c.localHostPort, hostPort == c.localHostPort)
	return hostPort == c.localHostPort, nil
}

const replicaSetName = "juju"

// startReplicaSet starts up a replica set with n mongo instances.
func startReplicaSet(n int) (_ []*testing.MgoInstance, err error) {
	insts := make([]*testing.MgoInstance, 0, n)
	root, err := newMongoInstance()
	if err != nil {
		return nil, err
	}
	insts = append(insts, root)
	defer func() {
		if err == nil {
			return
		}
		for _, inst := range insts {
			inst.Destroy()
		}
	}()

	dialInfo := root.DialInfo()
	dialInfo.Direct = true
	dialInfo.Timeout = 60 * time.Second

	session, err := root.DialDirect()
	if err != nil {
		return nil, fmt.Errorf("cannot dial root instance: %v", err)
	}
	defer session.Close()

	logger.Infof("dialled root instance")

	if err := replicaset.Initiate(session, root.Addr(), replicaSetName); err != nil {
		return nil, fmt.Errorf("cannot initiate replica set: %v", err)
	}
	var members []replicaset.Member
	for i := 1; i < n; i++ {
		inst, err := newMongoInstance()
		if err != nil {
			return nil, err
		}
		insts = append(insts, inst)
		members = append(members, replicaset.Member{
			Address:  inst.Addr(),
			Priority: newFloat64(0.1),
			Id:       i + 1,
		})
	}
	attempt := utils.AttemptStrategy{
		Total: 60 * time.Second,
		Delay: 1 * time.Second,
	}
	for a := attempt.Start(); a.Next(); {
		err := replicaset.Add(session, members...)
		if err == nil {
			break
		}
		logger.Errorf("cannot add members: %v", err)
		if !a.HasNext() {
			return nil, fmt.Errorf("timed out trying to add members")
		}
		logger.Errorf("retrying")
	}
	return insts, err
}

func newMongoInstance() (*testing.MgoInstance, error) {
	inst := &testing.MgoInstance{Params: []string{"--replSet", replicaSetName}}
	if err := inst.Start(true); err != nil {
		return nil, fmt.Errorf("cannot start mongo server: %s", err.Error())
	}
	return inst, nil
}

func newFloat64(f float64) *float64 {
	return &f
}

// connectionIsFatal returns a function suitable for passing
// as the isFatal argument to worker.NewRunner,
// that diagnoses an error as fatal if the connection
// has failed or if the error is otherwise fatal.
// Copied from jujud.
func connectionIsFatal(conn singular.Conn) func(err error) bool {
	return func(err error) bool {
		if err := conn.Ping(); err != nil {
			logger.Infof("error pinging %T: %v", conn, err)
			return true
		}
		logger.Infof("error %q is not fatal", err)
		return false
	}
}
