package internal

import (
	"context"
	"github.com/cenkalti/backoff/v4"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"io"
	"oxia/common"
	"oxia/proto"
	"sync"
	"time"
)

type ShardManager interface {
	io.Closer
	Start()
	Get(key string) uint32
	GetAll() []uint32
	Leader(shardId uint32) string
}

type shardManagerImpl struct {
	sync.Mutex
	shardStrategy  ShardStrategy
	clientPool     common.ClientPool
	serviceAddress string
	shards         map[uint32]Shard
	closeC         chan bool
	logger         zerolog.Logger
}

func NewShardManager(shardStrategy ShardStrategy, clientPool common.ClientPool, serviceAddress string) ShardManager {
	return &shardManagerImpl{
		shardStrategy:  shardStrategy,
		clientPool:     clientPool,
		serviceAddress: serviceAddress,
		shards:         make(map[uint32]Shard),
		closeC:         make(chan bool),
		logger:         log.With().Str("component", "shardManager").Logger(),
	}
}

func (s *shardManagerImpl) Close() error {
	close(s.closeC)
	return nil
}

func (s *shardManagerImpl) Start() {
	readyC := make(chan bool)

	ctx, cancel := context.WithCancel(context.Background())

	go common.DoWithLabels(map[string]string{
		"oxia": "receive-shard-updates",
	}, func() {
		s.receiveWithRecovery(ctx, readyC)
	})

	go common.DoWithLabels(map[string]string{
		"oxia": "cancel-shard-updates",
	}, func() {
		if _, ok := <-s.closeC; !ok {
			cancel()
		}
	})

	<-readyC
}

func (s *shardManagerImpl) Get(key string) uint32 {
	s.Lock()
	defer s.Unlock()

	predicate := s.shardStrategy.Get(key)

	for _, shard := range s.shards {
		if predicate(shard) {
			return shard.Id
		}
	}
	panic("shard not found")
}

func (s *shardManagerImpl) GetAll() []uint32 {
	s.Lock()
	defer s.Unlock()

	shardIds := make([]uint32, 0, len(s.shards))
	for shardId := range s.shards {
		shardIds = append(shardIds, shardId)
	}
	return shardIds
}

func (s *shardManagerImpl) Leader(shardId uint32) string {
	s.Lock()
	defer s.Unlock()

	if shard, ok := s.shards[shardId]; ok {
		return shard.Leader
	}
	panic("shard not found")
}

func (s *shardManagerImpl) isClosed() bool {
	select {
	case _, ok := <-s.closeC:
		if !ok {
			return true
		}
	default:
		//noop
	}
	return false
}

func (s *shardManagerImpl) receiveWithRecovery(ctx context.Context, readyC chan bool) {
	backOff := common.NewBackOff(ctx)
	err := backoff.RetryNotify(
		func() error {
			err := s.receive(backOff, ctx, readyC)
			if s.isClosed() {
				s.logger.Debug().Err(err).Msg("Closed")
				return nil
			}
			return err
		},
		backOff,
		func(err error, duration time.Duration) {
			if status.Code(err) != codes.Canceled {
				s.logger.Warn().Err(err).
					Dur("retry-after", duration).
					Msg("Failed receiving shard assignments, retrying later")
			}
		},
	)
	if err != nil {
		s.logger.Error().Err(err).Msg("Failed receiving shard assignments")
	}
}

func (s *shardManagerImpl) receive(backOff backoff.BackOff, ctx context.Context, readyC chan bool) error {
	rpc, err := s.clientPool.GetClientRpc(s.serviceAddress)
	if err != nil {
		return err
	}

	request := proto.ShardAssignmentsRequest{}

	stream, err := rpc.ShardAssignments(ctx, &request)
	if err != nil {
		return err
	}

	for {
		if response, err := stream.Recv(); err != nil {
			return err
		} else {
			shards := make([]Shard, len(response.Assignments))
			for i, assignment := range response.Assignments {
				shards[i] = toShard(assignment)
			}
			s.update(shards)
			readyC <- true
			backOff.Reset()
		}
	}
}

func (s *shardManagerImpl) update(updates []Shard) {
	s.Lock()
	defer s.Unlock()

	for _, update := range updates {
		if _, ok := s.shards[update.Id]; !ok {
			//delete overlaps
			for shardId, existing := range s.shards {
				if overlap(update.HashRange, existing.HashRange) {
					s.logger.Info().Msgf("Deleting shard %+v as it overlaps with %+v", existing, update)
					delete(s.shards, shardId)
				}
			}
		}
		s.shards[update.Id] = update
	}
}

func overlap(a HashRange, b HashRange) bool {
	return !(a.MinInclusive > b.MaxInclusive || a.MaxInclusive < b.MinInclusive)
}
