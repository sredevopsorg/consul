package autopilotevents

import (
	"fmt"
	"net"
	"sort"
	"time"

	"github.com/hashicorp/consul/acl"
	"github.com/hashicorp/consul/agent/consul/stream"
	"github.com/hashicorp/consul/agent/structs"
	"github.com/hashicorp/consul/types"
	autopilot "github.com/hashicorp/raft-autopilot"
)

const (
	EventTopicReadyServers stream.StringTopic = "ready-servers"
)

// ReadyServerInfo includes information about a server that is ready
// to handle incoming requests.
type ReadyServerInfo struct {
	ID              string
	Address         string
	TaggedAddresses map[string]string
	Version         string
}

func (info *ReadyServerInfo) Equal(other *ReadyServerInfo) bool {
	if info.ID != other.ID {
		return false
	}

	if info.Version != other.Version {
		return false
	}

	if info.Address != other.Address {
		return false
	}

	if len(info.TaggedAddresses) != len(other.TaggedAddresses) {
		return false
	}

	for tag, infoAddr := range info.TaggedAddresses {
		if otherAddr, ok := other.TaggedAddresses[tag]; !ok || infoAddr != otherAddr {
			return false
		}
	}

	return true
}

// EventPayloadReadyServers
type EventPayloadReadyServers []ReadyServerInfo

func (e EventPayloadReadyServers) Subject() stream.Subject { return stream.SubjectNone }

func (e EventPayloadReadyServers) HasReadPermission(authz acl.Authorizer) bool {
	// Any service in the mesh will need access to where the servers live. Therefore
	// we check if the authorizer grants permissions on any service and if so then
	// we allow seeing where the servers are.
	var authzContext acl.AuthorizerContext
	structs.WildcardEnterpriseMetaInPartition(structs.WildcardSpecifier).
		FillAuthzContext(&authzContext)

	return authz.ServiceWriteAny(&authzContext) == acl.Allow
}

func ExtractEventPayload(event stream.Event) (EventPayloadReadyServers, error) {
	if event.Topic != EventTopicReadyServers {
		return nil, fmt.Errorf("unexpected topic (%q) for a %q event", event.Topic, EventTopicReadyServers)
	}

	if payload, ok := event.Payload.(EventPayloadReadyServers); ok {
		return payload, nil
	}

	return nil, fmt.Errorf("unexpected payload type %T for %q event", event.Payload, EventTopicReadyServers)
}

type Config struct {
	Autopilot    Autopilot
	GetStore     func() StateStore
	Publisher    Publisher
	timeProvider timeProvider
}

// ReadyServersEventPublisher is capable to tracking changes to ready servers
// between consecutive calls to PublishReadyServersEvents. It will then publish
// "ready-servers" events as necessary.
type ReadyServersEventPublisher struct {
	Config
	previous EventPayloadReadyServers
}

func NewReadyServersEventPublisher(config Config) *ReadyServersEventPublisher {
	return &ReadyServersEventPublisher{Config: config}
}

//go:generate mockery -name Autopilot -inpkg -testonly
type Autopilot interface {
	GetState() *autopilot.State
}

//go:generate mockery -name StateStore -inpkg -testonly
type StateStore interface {
	GetNodeID(types.NodeID, *acl.EnterpriseMeta) (uint64, *structs.Node, error)
}

//go:generate mockery -name Publisher -inpkg -testonly
type Publisher interface {
	Publish([]stream.Event)
}

//go:generate mockery -name timeProvider -inpkg -testonly
type timeProvider interface {
	Now() time.Time
}

// PublishReadyServersEvents will publish a "ready-servers" event if the list of
// ready servers has changed since the last time events were published.
func (r *ReadyServersEventPublisher) PublishReadyServersEvents(state *autopilot.State) {
	if events, ok := r.readyServersEvents(state); ok {
		// if the event publisher were to not be able to keep up with procesing events
		// then its possible this blocks. It could cause autopilot to not update its
		// state as often as it should. However if this blocks for over 10s then
		// not updating the autopilot state as quickly is likely the least of our
		// concerns. If we need to make this async then we probably need to single
		// flight these to ensure proper event ordering.
		r.Publisher.Publish(events)
	}
}

func (r *ReadyServersEventPublisher) readyServersEvents(state *autopilot.State) ([]stream.Event, bool) {
	// First, we need to pull all the ready servers out from the autopilot state.
	servers := r.autopilotStateToReadyServers(state)

	// Next we, sort the servers list to make comparison easier later on. We do
	// this outside of the next length check conditional block to ensure that all
	// values of previousReadyServers we store will be sorted and the future
	// comparison's will remain valid.
	sort.Slice(servers, func(i, j int) bool {
		// no two servers can have the same id so this is sufficient
		return servers[i].ID < servers[j].ID
	})

	// If the number of ready servers hasn't changed then we need to inspect individual
	// servers to see if there are differences. If the number of servers has changed
	// we know that an event should be generated and sent.
	if len(r.previous) == len(servers) {
		diff := false
		// We are relying on the fact that both of the slices will be sorted and that
		// we don't care what the actual differences are but instead just that they
		// have differences. We can then just iterate through all elements of both
		// slices at the same time and check the elements for equality. Also,
		// this is taking advantage of Go being able to do equality checking on
		// structs without having to check all the fields separately. If we
		// were to add some nested fields and pointers then we may need to alter
		// this equality checking.
		for i := 0; i < len(servers); i++ {
			if !r.previous[i].Equal(&servers[i]) {
				diff = true
				break
			}
		}

		// The list of ready servers is identical to the previous ones. Therefore
		// we will not send any event.
		if !diff {
			return nil, false
		}
	}

	r.previous = servers

	return []stream.Event{r.newReadyServersEvent(servers)}, true
}

// autopilotStateToReadyServers will iterate through all servers in the autopilot
// state and compile a list of servers which are "ready". Readiness means that
// they would be an acceptable target for stale queries.
func (r *ReadyServersEventPublisher) autopilotStateToReadyServers(state *autopilot.State) EventPayloadReadyServers {
	var servers EventPayloadReadyServers
	for _, srv := range state.Servers {
		// All healthy servers are caught up enough to be included in a ready servers.
		// Servers with voting rights that are still healthy according to Serf are
		// also included as they have likely just fallen behind the leader a little
		// after initially replicating state. They are still acceptable targets
		// for most stale queries and clients can bound the staleness if necessary.
		// Including them is a means to prevent flapping the list of servers we
		// advertise as ready and flooding the network with notifications to all
		// dataplanes of server updates.
		//
		// TODO (agentless) for a non-voting server that is still alive but fell
		// behind, should we cause it to be removed. For voters we know they were caught
		// up at some point but for non-voters we cannot know the same thing.
		if srv.Health.Healthy || (srv.HasVotingRights() && srv.Server.NodeStatus == autopilot.NodeAlive) {
			// autopilot information contains addresses in the <host>:<port> form. We only care about the
			// the host so we parse it out here and discard the port.
			host, err := extractHost(string(srv.Server.Address))
			if err != nil || host == "" {

				continue
			}

			servers = append(servers, ReadyServerInfo{
				ID:              string(srv.Server.ID),
				Address:         host,
				Version:         srv.Server.Version,
				TaggedAddresses: r.getTaggedAddresses(srv),
			})
		}
	}

	return servers
}

// getTaggedAddresses will get the tagged addresses for the given server or return nil
// if it encounters an error or unregistered server.
func (r *ReadyServersEventPublisher) getTaggedAddresses(srv *autopilot.ServerState) map[string]string {
	// we have no callback to lookup the tagged addresses so we can return early
	if r.GetStore == nil {
		return nil
	}

	// Assuming we have been provided a callback to get a state store implementation, then
	// we will attempt to lookup the node for the autopilot server. We use this to get the
	// tagged addresses so that consumers of these events will be able to distinguish LAN
	// vs WAN addresses as well as IP protocol differentiation. At first I thought we may
	// need to hook into catalog events so that if the tagged addresses change then
	// we can synthesize new events. That would be pretty complex so this code does not
	// deal with that. The reasoning why that is probably okay is that autopilot will
	// send us the state at least once every 30s. That means that we will grab the nodes
	// from the catalog at that often and publish the events. So while its not quite
	// as responsive as actually watching for the Catalog changes, its MUCH simpler to
	// code and reason about and having those addresses be updated within 30s is good enough.
	_, node, err := r.GetStore().GetNodeID(types.NodeID(srv.Server.ID), structs.NodeEnterpriseMetaInDefaultPartition())
	if err != nil || node == nil {
		// no catalog information means we should return a nil addres map
		return nil
	}

	if len(node.TaggedAddresses) == 0 {
		return nil
	}

	addrs := make(map[string]string)
	for tag, address := range node.TaggedAddresses {
		// just like for the Nodes main Address, we only care about the IPs and not the
		// port so we parse the host out and discard the port.
		host, err := extractHost(address)
		if err != nil || host == "" {
			continue
		}
		addrs[tag] = host
	}

	return addrs
}

// newReadyServersEvent will create a stream.Event with the provided ready server info.
func (r *ReadyServersEventPublisher) newReadyServersEvent(servers EventPayloadReadyServers) stream.Event {
	now := time.Now()
	if r.timeProvider != nil {
		now = r.timeProvider.Now()
	}
	return stream.Event{
		Topic:   EventTopicReadyServers,
		Index:   uint64(now.UnixMicro()),
		Payload: servers,
	}
}

// HandleSnapshot is the EventPublisher callback to generate a snapshot for the "ready-servers" event streams.
func (r *ReadyServersEventPublisher) HandleSnapshot(_ stream.SubscribeRequest, buf stream.SnapshotAppender) (uint64, error) {
	state := r.Autopilot.GetState()
	if state == nil {
		return 0, fmt.Errorf("autopilot state is uninitialized")
	}

	servers := r.autopilotStateToReadyServers(state)
	sort.Slice(servers, func(i, j int) bool {
		// no two servers can have the same id so this is sufficient
		return servers[i].ID < servers[j].ID
	})

	event := r.newReadyServersEvent(servers)
	buf.Append([]stream.Event{event})
	return event.Index, nil
}

// extractHost is a small convenience function to catch errors regarding
// missing ports from the net.SplitHostPort function.
func extractHost(addr string) (string, error) {
	host, _, err := net.SplitHostPort(addr)
	if err == nil {
		return host, nil
	}
	if ae, ok := err.(*net.AddrError); ok && ae.Err == "missing port in address" {
		return addr, nil
	}
	return "", err
}
