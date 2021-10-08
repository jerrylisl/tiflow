// Copyright 2021 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package p2p

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/pingcap/failpoint"
	"github.com/pingcap/log"
	cerrors "github.com/pingcap/ticdc/pkg/errors"
	"github.com/pingcap/ticdc/pkg/security"
	"github.com/pingcap/ticdc/pkg/util/testleak"
	"github.com/pingcap/ticdc/proto/p2p"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

func newServerForIntegrationTesting(t *testing.T, serverID string) (server *MessageServer, addr string, cancel func()) {
	addr = t.TempDir() + "/p2p-testing.sock"
	lis, err := net.Listen("unix", addr)
	require.NoError(t, err)

	var opts []grpc.ServerOption
	grpcServer := grpc.NewServer(opts...)

	server = NewMessageServer(SenderID(serverID))
	p2p.RegisterCDCPeerToPeerServer(grpcServer, server)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = grpcServer.Serve(lis)
	}()

	cancel = func() {
		grpcServer.Stop()
		wg.Wait()
	}
	return
}

func runP2PIntegrationTest(ctx context.Context, t *testing.T, size int, numTopics int) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	server, addr, cancelServer := newServerForIntegrationTesting(t, "test-server-1")
	defer cancelServer()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			err := server.Run(ctx)
			if cerrors.ErrPeerMessageInjectedServerRestart.Equal(err) {
				log.Warn("server restarted")
				continue
			}
			require.Regexp(t, ".*context canceled.*", err.Error())
			break
		}
	}()

	for j := 0; j < numTopics; j++ {
		topicName := fmt.Sprintf("test-topic-%d", j)
		var lastIndex int64
		errCh := mustAddHandler(ctx, t, server, topicName, &testTopicContent{}, func(senderID string, i interface{}) error {
			require.Equal(t, "test-client-1", senderID)
			require.IsType(t, &testTopicContent{}, i)
			content := i.(*testTopicContent)
			require.Equal(t, content.Index-1, lastIndex)
			lastIndex = content.Index
			return nil
		})

		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-ctx.Done():
			case err := <-errCh:
				require.NoError(t, err)
			}
		}()
	}

	client := NewMessageClient("test-client-1")
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := client.Run(ctx, "unix", addr, "test-server-1", &security.Credential{})
		if err != nil {
			log.Warn("client returned error", zap.Error(err))
			require.Regexp(t, ".*context canceled.*", err.Error())
		}
	}()

	var wg1 sync.WaitGroup
	wg1.Add(numTopics)
	for j := 0; j < numTopics; j++ {
		topicName := fmt.Sprintf("test-topic-%d", j)
		go func() {
			defer wg1.Done()
			var oldSeq Seq
			for i := 0; i < size; i++ {
				content := &testTopicContent{Index: int64(i + 1)}
				seq, err := client.SendMessage(ctx, Topic(topicName), content)
				require.NoError(t, err)
				require.Equal(t, oldSeq+1, seq)
				oldSeq = seq
			}

			require.Eventuallyf(t, func() bool {
				seq, ok := client.CurrentAck(Topic(topicName))
				if !ok {
					return false
				}
				return seq >= Seq(size)
			}, time.Second*10, time.Millisecond*20, "failed to wait for ack")
		}()
	}

	wg1.Wait()
	cancel()
	wg.Wait()
}

func TestMessageClientBasic(t *testing.T) {
	defer testleak.AfterTestT(t)()

	ctx, cancel := context.WithTimeout(context.TODO(), defaultTimeout)
	defer cancel()

	runP2PIntegrationTest(ctx, t, defaultMessageBatchSizeLarge, 1)
}

func TestMessageClientBasicMultiTopics(t *testing.T) {
	defer testleak.AfterTestT(t)()

	ctx, cancel := context.WithTimeout(context.TODO(), defaultTimeout)
	defer cancel()

	runP2PIntegrationTest(ctx, t, defaultMessageBatchSizeLarge, 32)
}

func TestMessageClientServerRestart(t *testing.T) {
	defer testleak.AfterTestT(t)()

	_ = failpoint.Enable("github.com/pingcap/ticdc/pkg/p2p/ServerInjectServerRestart", "1%return(true)")
	defer func() {
		_ = failpoint.Disable("github.com/pingcap/ticdc/pkg/p2p/ServerInjectServerRestart")
	}()

	ctx, cancel := context.WithTimeout(context.TODO(), defaultTimeout)
	defer cancel()

	runP2PIntegrationTest(ctx, t, defaultMessageBatchSizeLarge, 1)
}

func TestMessageClientServerRestartMultiTopics(t *testing.T) {
	defer testleak.AfterTestT(t)()

	_ = failpoint.Enable("github.com/pingcap/ticdc/pkg/p2p/ServerInjectServerRestart", "3%return(true)")
	defer func() {
		_ = failpoint.Disable("github.com/pingcap/ticdc/pkg/p2p/ServerInjectServerRestart")
	}()

	ctx, cancel := context.WithTimeout(context.TODO(), defaultTimeout*4)
	defer cancel()

	runP2PIntegrationTest(ctx, t, defaultMessageBatchSizeSmall, 4)
}

func TestMessageClientRestart(t *testing.T) {
	defer testleak.AfterTestT(t)()

	_ = failpoint.Enable("github.com/pingcap/ticdc/pkg/p2p/ClientInjectStreamFailure", "10%return(true)")
	defer func() {
		_ = failpoint.Disable("github.com/pingcap/ticdc/pkg/p2p/ClientInjectStreamFailure")
	}()

	ctx, cancel := context.WithTimeout(context.TODO(), defaultTimeout)
	defer cancel()

	runP2PIntegrationTest(ctx, t, defaultMessageBatchSizeMedium, 1)
}

func TestMessageClientRestartMultiTopics(t *testing.T) {
	defer testleak.AfterTestT(t)()

	_ = failpoint.Enable("github.com/pingcap/ticdc/pkg/p2p/ClientInjectStreamFailure", "3%return(true)")
	defer func() {
		_ = failpoint.Disable("github.com/pingcap/ticdc/pkg/p2p/ClientInjectStreamFailure")
	}()

	ctx, cancel := context.WithTimeout(context.TODO(), defaultTimeout)
	defer cancel()

	runP2PIntegrationTest(ctx, t, defaultMessageBatchSizeLarge, 32)
}

func TestMessageClientBasicNonblocking(t *testing.T) {
	defer testleak.AfterTestT(t)()

	ctx, cancel := context.WithTimeout(context.TODO(), defaultTimeout)
	defer cancel()

	server, addr, cancelServer := newServerForIntegrationTesting(t, "test-server-1")
	defer cancelServer()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := server.Run(ctx)
		if err != nil {
			require.Regexp(t, ".*context canceled.*", err.Error())
		}
	}()

	var lastIndex int64
	errCh := mustAddHandler(ctx, t, server, "test-topic-1", &testTopicContent{}, func(senderID string, i interface{}) error {
		require.Equal(t, "test-client-1", senderID)
		require.IsType(t, &testTopicContent{}, i)
		content := i.(*testTopicContent)
		swapped := atomic.CompareAndSwapInt64(&lastIndex, content.Index-1, content.Index)
		require.True(t, swapped)
		return nil
	})

	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
		case err := <-errCh:
			require.NoError(t, err)
		}
	}()

	client := NewMessageClient("test-client-1")
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := client.Run(ctx, "unix", addr, "test-server-1", &security.Credential{})
		require.Error(t, err)
		require.Regexp(t, ".*context canceled.*", err.Error())
	}()

	var oldSeq Seq
	for i := 0; i < defaultMessageBatchSizeSmall; i++ {
		content := &testTopicContent{Index: int64(i + 1)}
		var (
			seq Seq
			err error
		)
		require.Eventually(t, func() bool {
			seq, err = client.TrySendMessage(ctx, "test-topic-1", content)
			return !cerrors.ErrPeerMessageSendTryAgain.Equal(err)
		}, time.Second*5, time.Millisecond*10)
		require.NoError(t, err)
		require.Equal(t, oldSeq+1, seq)
		oldSeq = seq
	}

	require.Eventually(t, func() bool {
		seq, ok := client.CurrentAck("test-topic-1")
		if !ok {
			return false
		}
		return seq >= defaultMessageBatchSizeSmall
	}, time.Second*10, time.Millisecond*20)

	cancel()
	wg.Wait()
}