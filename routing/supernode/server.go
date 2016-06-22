package supernode

import (
	"errors"
	"fmt"

	key "github.com/ipfs/go-ipfs/blocks/key"
	dhtpb "github.com/ipfs/go-ipfs/routing/dht/pb"
	record "github.com/ipfs/go-ipfs/routing/record"
	proxy "github.com/ipfs/go-ipfs/routing/supernode/proxy"
	datastore "gx/ipfs/QmbCg24DeRKaRDLHbzzSVj7xndmWCPanBLkAM7Lx2nbrFs/go-datastore"

	pstore "gx/ipfs/QmQdnfvZQuhdT93LNc5bos52wAmdr3G2p6G8teLJMEN32P/go-libp2p-peerstore"
	peer "gx/ipfs/QmRBqJF7hb8ZSpRcMwUt8hNhydWcxGEhtk81HKq6oUwKvs/go-libp2p-peer"
	proto "gx/ipfs/QmZ4Qi3GaRbjcx28Sme5eMH7RQjGkt8wHxt2a65oLaeFEV/gogo-protobuf/proto"
	context "gx/ipfs/QmZy2y8t9zQH2a1b8q2ZSLKp17ATuJoCNxxyMFG5qFExpt/go-net/context"
)

// Server handles routing queries using a database backend
type Server struct {
	local           peer.ID
	routingBackend  datastore.Datastore
	peerstore       pstore.Peerstore
	*proxy.Loopback // so server can be injected into client
}

// NewServer creates a new Supernode routing Server
func NewServer(ds datastore.Datastore, ps pstore.Peerstore, local peer.ID) (*Server, error) {
	s := &Server{local, ds, ps, nil}
	s.Loopback = &proxy.Loopback{
		Handler: s,
		Local:   local,
	}
	return s, nil
}

func (_ *Server) Bootstrap(ctx context.Context) error {
	return nil
}

// HandleLocalRequest implements the proxy.RequestHandler interface. This is
// where requests are received from the outside world.
func (s *Server) HandleRequest(ctx context.Context, p peer.ID, req *dhtpb.Message) *dhtpb.Message {
	_, response := s.handleMessage(ctx, p, req) // ignore response peer. it's local.
	return response
}

func (s *Server) handleMessage(
	ctx context.Context, p peer.ID, req *dhtpb.Message) (peer.ID, *dhtpb.Message) {

	defer log.EventBegin(ctx, "routingMessageReceived", req, p).Done()

	var response = dhtpb.NewMessage(req.GetType(), req.GetKey(), req.GetClusterLevel())
	switch req.GetType() {

	case dhtpb.Message_GET_VALUE:
		rawRecord, err := getRoutingRecord(s.routingBackend, key.Key(req.GetKey()))
		if err != nil {
			return "", nil
		}
		response.Record = rawRecord
		return p, response

	case dhtpb.Message_PUT_VALUE:
		// FIXME: verify complains that the peer's ID is not present in the
		// peerstore. Mocknet problem?
		// if err := verify(s.peerstore, req.GetRecord()); err != nil {
		// 	log.Event(ctx, "validationFailed", req, p)
		// 	return "", nil
		// }
		putRoutingRecord(s.routingBackend, key.Key(req.GetKey()), req.GetRecord())
		return p, req

	case dhtpb.Message_FIND_NODE:
		p := s.peerstore.PeerInfo(peer.ID(req.GetKey()))
		pri := []dhtpb.PeerRoutingInfo{
			{
				PeerInfo: p,
				// Connectedness: TODO
			},
		}
		response.CloserPeers = dhtpb.PeerRoutingInfosToPBPeers(pri)
		return p.ID, response

	case dhtpb.Message_ADD_PROVIDER:
		for _, provider := range req.GetProviderPeers() {
			providerID := peer.ID(provider.GetId())
			if providerID == p {
				store := []*dhtpb.Message_Peer{provider}
				storeProvidersToPeerstore(s.peerstore, p, store)
				if err := putRoutingProviders(s.routingBackend, key.Key(req.GetKey()), store); err != nil {
					return "", nil
				}
			} else {
				log.Event(ctx, "addProviderBadRequest", p, req)
			}
		}
		return "", nil

	case dhtpb.Message_GET_PROVIDERS:
		providers, err := getRoutingProviders(s.routingBackend, key.Key(req.GetKey()))
		if err != nil {
			return "", nil
		}
		response.ProviderPeers = providers
		return p, response

	case dhtpb.Message_PING:
		return p, req
	default:
	}
	return "", nil
}

var _ proxy.RequestHandler = &Server{}
var _ proxy.Proxy = &Server{}

func getRoutingRecord(ds datastore.Datastore, k key.Key) (*dhtpb.Record, error) {
	dskey := k.DsKey()
	val, err := ds.Get(dskey)
	if err != nil {
		return nil, err
	}
	recordBytes, ok := val.([]byte)
	if !ok {
		return nil, fmt.Errorf("datastore had non byte-slice value for %v", dskey)
	}
	var record dhtpb.Record
	if err := proto.Unmarshal(recordBytes, &record); err != nil {
		return nil, errors.New("failed to unmarshal dht record from datastore")
	}
	return &record, nil
}

func putRoutingRecord(ds datastore.Datastore, k key.Key, value *dhtpb.Record) error {
	data, err := proto.Marshal(value)
	if err != nil {
		return err
	}
	dskey := k.DsKey()
	// TODO namespace
	if err := ds.Put(dskey, data); err != nil {
		return err
	}
	return nil
}

func putRoutingProviders(ds datastore.Datastore, k key.Key, newRecords []*dhtpb.Message_Peer) error {
	log.Event(context.Background(), "putRoutingProviders", &k)
	oldRecords, err := getRoutingProviders(ds, k)
	if err != nil {
		return err
	}
	mergedRecords := make(map[string]*dhtpb.Message_Peer)
	for _, provider := range oldRecords {
		mergedRecords[provider.GetId()] = provider // add original records
	}
	for _, provider := range newRecords {
		mergedRecords[provider.GetId()] = provider // overwrite old record if new exists
	}
	var protomsg dhtpb.Message
	protomsg.ProviderPeers = make([]*dhtpb.Message_Peer, 0, len(mergedRecords))
	for _, provider := range mergedRecords {
		protomsg.ProviderPeers = append(protomsg.ProviderPeers, provider)
	}
	data, err := proto.Marshal(&protomsg)
	if err != nil {
		return err
	}
	return ds.Put(providerKey(k), data)
}

func storeProvidersToPeerstore(ps pstore.Peerstore, p peer.ID, providers []*dhtpb.Message_Peer) {
	for _, provider := range providers {
		providerID := peer.ID(provider.GetId())
		if providerID != p {
			log.Errorf("provider message came from third-party %s", p)
			continue
		}
		for _, maddr := range provider.Addresses() {
			// as a router, we want to store addresses for peers who have provided
			ps.AddAddr(p, maddr, pstore.AddressTTL)
		}
	}
}

func getRoutingProviders(ds datastore.Datastore, k key.Key) ([]*dhtpb.Message_Peer, error) {
	e := log.EventBegin(context.Background(), "getProviders", &k)
	defer e.Done()
	var providers []*dhtpb.Message_Peer
	if v, err := ds.Get(providerKey(k)); err == nil {
		if data, ok := v.([]byte); ok {
			var msg dhtpb.Message
			if err := proto.Unmarshal(data, &msg); err != nil {
				return nil, err
			}
			providers = append(providers, msg.GetProviderPeers()...)
		}
	}
	return providers, nil
}

func providerKey(k key.Key) datastore.Key {
	return datastore.KeyWithNamespaces([]string{"routing", "providers", k.String()})
}

func verify(ps pstore.Peerstore, r *dhtpb.Record) error {
	v := make(record.Validator)
	v["pk"] = record.PublicKeyValidator
	p := peer.ID(r.GetAuthor())
	pk := ps.PubKey(p)
	if pk == nil {
		return fmt.Errorf("do not have public key for %s", p)
	}
	if err := record.CheckRecordSig(r, pk); err != nil {
		return err
	}
	if err := v.VerifyRecord(r); err != nil {
		return err
	}
	return nil
}
