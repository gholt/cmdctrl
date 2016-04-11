package cmdctrl2

import (
	"net"
	"sync"
	"time"

	"github.com/gholt/flog"
	pb "github.com/pandemicsyn/cmdctrl2/api"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Service is what the Server will control.
type Service interface {
	// Start starts the service and returns a channel that is closed when the
	// service stops. This channel should be used just for this launching of
	// the service. For example, two Start() calls with no Stop() might return
	// the same channel, but after Stop() the next Start() should return a new
	// channel (since the first channel should have been closed).
	Start() <-chan struct{}
	// Stop stops the service and closes any channel Start() previously
	// returned.
	Stop()
	// RingUpdate notifies the service of a new ring; the service should return
	// the version of the ring that will be in use after this call, which might
	// be newer than the ring given to this call, but should not be older.
	RingUpdate(version int64, ringBytes []byte) int64
	// Stats asks the service to return the current values of any metrics it's
	// tracking.
	Stats() []byte
	// HealthCheck returns true if running, or false and a message as to the
	// reason why not.
	HealthCheck() (bool, string)
}

// Config contains options for new CmdCtrl Servers.
type Config struct {
	// ListenAddress is the ip:port for the CmdCtrl GRPC to listen on; if
	// empty, GRPC will be not be started.
	ListenAddress string
	// CertFile is the path to the TLS certificate file for the GRPC service;
	// if empty, GRPC will be not be started.
	CertFile string
	// KeyFile is the path to the TLS key file for the GRPC service; if empty,
	// GRPC will be not be started.
	KeyFile string
}

// Server will control the Service, as well as its own CmdCtrl GRPC service.
type Server struct {
	service     Service
	grpcService *grpcService

	lock     sync.Mutex
	doneChan chan struct{}
}

// New returns a new Server for use; the Start method will not have been
// called.
func New(c *Config, s Service) *Server {
	srv := &Server{service: s}
	if c.ListenAddress != "" && c.CertFile != "" && c.KeyFile != "" {
		srv.grpcService = &grpcService{
			service:    s,
			listenAddr: c.ListenAddress,
			certFile:   c.CertFile,
			keyFile:    c.KeyFile,
		}
	}
	return srv
}

// Start starts the server and returns a channel that is closed when the server
// stops.
func (s *Server) Start() <-chan struct{} {
	var retChan chan struct{}
	s.lock.Lock()
	if s.doneChan == nil {
		serviceDoneChan := s.service.Start()
		var grpcDoneChan <-chan struct{}
		if s.grpcService != nil {
			grpcDoneChan = s.grpcService.startGRPC()
		}
		s.doneChan = make(chan struct{})
		go func(a <-chan struct{}, b <-chan struct{}, s func(), c chan<- struct{}) {
			if a != nil && b != nil {
				select {
				case <-a:
				case <-b:
				}
			} else if a != nil {
				<-a
			} else if b != nil {
				<-b
			}
			s()
			close(c)
		}(grpcDoneChan, serviceDoneChan, s.Stop, s.doneChan)
	}
	retChan = s.doneChan
	s.lock.Unlock()
	return retChan
}

// Stop stops the server.
func (s *Server) Stop() {
	var doneChan chan struct{}
	s.lock.Lock()
	if s.doneChan != nil {
		s.service.Stop()
		if s.grpcService != nil {
			s.grpcService.stopGRPC()
		}
		doneChan = s.doneChan
		s.doneChan = nil
	}
	s.lock.Unlock()
	if doneChan != nil {
		<-doneChan
	}
}

type grpcService struct {
	service    Service
	listenAddr string
	certFile   string
	keyFile    string

	lock     sync.Mutex
	doneChan chan struct{}
	listener net.Listener
	server   *grpc.Server
}

func (s *grpcService) startGRPC() <-chan struct{} {
	var retChan chan struct{}
	s.lock.Lock()
	if s.doneChan != nil {
		select {
		case <-s.doneChan: // Means previous run has stopped
			s.doneChan = nil
			s.listener = nil
			s.server = nil
		default: // Means previous run is still going
		}
	}
	if s.doneChan == nil {
		creds, err := credentials.NewServerTLSFromFile(s.certFile, s.keyFile)
		if err != nil {
			flog.CriticalPrintln("Unable to load CmdCtrl GRPC credentials:", err)
		} else {
			s.listener, err = net.Listen("tcp", s.listenAddr)
			if err != nil {
				flog.CriticalPrintln("Unable to start CmdCtrl GRPC:", err)
			} else {
				s.server = grpc.NewServer(grpc.Creds(creds))
				pb.RegisterCmdCtrlServer(s.server, s)
				s.doneChan = make(chan struct{})
				go func(g *grpc.Server, l net.Listener, c chan<- struct{}) {
					if err := g.Serve(l); err != nil {
						flog.ErrorPrintln("CmdCtrl GRPC Server error:", err)
					}
					g.Stop()
					l.Close()
					close(c)
				}(s.server, s.listener, s.doneChan)
			}
		}
	}
	retChan = s.doneChan
	if retChan == nil {
		retChan = make(chan struct{})
		close(retChan)
	}
	s.lock.Unlock()
	return retChan
}

func (s *grpcService) stopGRPC() {
	s.lock.Lock()
	if s.doneChan != nil {
		s.server.Stop()
		s.listener.Close()
		<-s.doneChan
		s.doneChan = nil
		s.listener = nil
		s.server = nil
	}
	s.lock.Unlock()
}

func (s *grpcService) Stop(c context.Context, r *pb.EmptyMsg) (*pb.EmptyMsg, error) {
	s.service.Stop()
	return &pb.EmptyMsg{}, nil
}

func (s *grpcService) RingUpdate(c context.Context, r *pb.Ring) (*pb.RingUpdateResult, error) {
	res := pb.RingUpdateResult{}
	res.Newversion = s.service.RingUpdate(r.Version, r.Ring)
	return &res, nil

}

func (s *grpcService) Stats(c context.Context, r *pb.EmptyMsg) (*pb.StatsMsg, error) {
	return &pb.StatsMsg{Stats: s.service.Stats()}, nil
}

func (s *grpcService) HealthCheck(c context.Context, r *pb.EmptyMsg) (*pb.HealthCheckMsg, error) {
	hm := &pb.HealthCheckMsg{Ts: time.Now().Unix()}
	hm.Status, hm.Msg = s.service.HealthCheck()
	return hm, nil
}
