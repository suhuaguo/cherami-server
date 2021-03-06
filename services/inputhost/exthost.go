// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package inputhost

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pborman/uuid"
	"github.com/uber-common/bark"
	"github.com/uber/tchannel-go/thrift"

	"github.com/uber/cherami-thrift/.generated/go/admin"
	"github.com/uber/cherami-thrift/.generated/go/cherami"
	"github.com/uber/cherami-thrift/.generated/go/controller"
	"github.com/uber/cherami-thrift/.generated/go/shared"
	"github.com/uber/cherami-thrift/.generated/go/store"
	"github.com/uber/cherami-server/common"
	"github.com/uber/cherami-server/services/inputhost/load"
)

type (
	extHost struct {
		streams map[storeHostPort]*replicaInfo // Write protected by lk

		// channel to notify the ack aggregator that there's an in-flight message awaiting replica acnowledgements
		replyClientCh chan writeResponse

		// channel to the replicaconnection
		putMessagesCh <-chan *inPutMessage

		// channel to notify the path cache that this exthost is going down
		// once the pathCache gets this message, he will disconnect clients if all extents are down
		notifyExtCacheClosedCh chan string
		// channel to notify the path cache to completely unload the extent
		notifyExtCacheUnloadCh chan string
		extUUID                string
		destUUID               string
		destType               shared.DestinationType
		loadReporter           common.LoadReporterDaemon
		logger                 bark.Logger
		tClients               common.ClientFactory
		closeChannel           chan struct{}
		streamClosedChannel    chan struct{}
		numReplicas            int
		seqNo                  int64      // monotonic sequence number for the messages on this extent
		lastSuccessSeqNo       int64      // last sequence number where we replied success
		lastSuccessSeqNoCh     chan int64 // last sequence number where we replied success
		lastSentWatermark      int64      // last watermark sent to the replicas

		waitWriteWG   sync.WaitGroup
		waitReadWG    sync.WaitGroup
		shutdownWG    *sync.WaitGroup
		forceUnloadCh chan struct{} // this channel is used to make sure we don't wait for the unload timeout when an extent is closed

		extTokenBucketValue      atomic.Value // Value to controll access for TB for rate limit this extent
		extentMsgsLimitPerSecond int32        //per second rate limit for this extent
		lk                       sync.Mutex
		opened                   bool // Read/write protected by lk
		closed                   bool // Read/write protected by lk

		sealLk sync.Mutex // this lock is to make sure we just have one seal going on
		sealed uint32

		// maxSequence number is the maximum seq no which is allowed per extent
		// after that the extent will be sealed
		// we will seal at this sequence number pro-actively so that we don't
		// have a very large extent. We will seal at a random number between
		// 10 million and 20 million
		maxSequenceNumber int64

		// lastEnqueueTime is the enqueue-time that was stamped on the last message
		// published on this extent. this is used to validate and protect against the
		// system clock potentially going backwards -- so the enqueue-time stamped is
		// still strictly greater-than-or-equal to the previous message's enqueue-time.
		lastEnqueueTime int64

		limitsEnabled bool

		// load metrics that get reported
		// to the controller and/or m3
		extMetrics              *load.ExtentMetrics
		dstMetrics              *load.DstMetrics
		hostMetrics             *load.HostMetrics
		lastExtLoadReportedTime int64 // unix nanos when the last extent metrics were reported

		minimumAllowedMessageDelaySeconds int32 // min delay on messages
	}

	// Holds a particular extent for use by multiple publisher connections.
	// This is the cache member, not the cache. See extentCache in inputhost_util
	inExtentCache struct {
		extUUID    extentUUID
		connection *extHost
	}

	// writeResponse is kept internally by extHost to aggregate (append) the ACKs for a particular message from the
	// several replicas (see replicaconnection). The same structure is sent to each replica in replicaconnection.
	writeResponse struct {
		ackID        string
		seqNo        int64
		appendMsgAck chan *store.AppendMessageAck
		putMsgAck    chan<- *cherami.PutMessageAck
		sentTime     time.Time
		userContext  map[string]string // user specified context to pass through
	}

	// replicaInfo keeps track of both the replicaConnection and a timer object for this connection
	replicaInfo struct {
		conn      *replicaConnection
		sendTimer *common.Timer
	}

	extCacheClosedCb   func(string)
	extCacheUnloadedCb func(string)
)

const (
	// thriftCallTimeout is the timeout for the thrift context
	thriftCallTimeout = 1 * time.Minute

	// replicaSendTimeout is the timeout for the send to go through to the replica
	replicaSendTimeout = 1 * time.Minute

	// extIdleTimeout is the idle timeout for an extent. If we don't get any messages until this time, we close the extent
	extIdleTimeout = 1800 * time.Second // 30 minutes

	// logTimeout is the timeout at which we log the channel buffer size
	logTimeout = 1 * time.Minute

	// unloadTimeout is the timeout until which we keep the extent loaded
	unloadTimeout = 2 * time.Minute

	// maxTBSleepDuration is the max sleep duration for the rate limiter
	maxTBSleepDuration = 1 * time.Second

	// extLoadReportingInterval is the interval destination extent load is reported to controller
	extLoadReportingInterval = 2 * time.Second
)

var (
	// ErrTimeout is returned when the host is already shutdown
	ErrTimeout = &cherami.InternalServiceError{Message: "sending message to replica timed out"}

	// nullTime is an empty time struct
	nullTime time.Time

	// open is to indicate the extent is still open and we have not yet notified the controller
	open uint32

	// sealed is to indicate we have already sent the seal notification
	sealed uint32 = 1

	// msgAckTimeout is the time to wait for the ack from the replicas
	msgAckTimeout = 1 * time.Minute
)

func newExtConnection(destUUID string, pathCache *inPathCache, extUUID string, numReplicas int, loadReporterFactory common.LoadReporterDaemonFactory, logger bark.Logger, tClients common.ClientFactory, shutdownWG *sync.WaitGroup, limitsEnabled bool) *extHost {
	conn := &extHost{
		streams:                 make(map[storeHostPort]*replicaInfo),
		extUUID:                 extUUID,
		destUUID:                destUUID,
		destType:                pathCache.destType,
		logger:                  logger.WithField(common.TagModule, `extHost`),
		tClients:                tClients,
		lastSuccessSeqNo:        int64(-1),
		lastSuccessSeqNoCh:      nil,
		notifyExtCacheClosedCh:  pathCache.notifyExtHostCloseCh,
		notifyExtCacheUnloadCh:  pathCache.notifyExtHostUnloadCh,
		putMessagesCh:           pathCache.putMsgCh,
		replyClientCh:           make(chan writeResponse, defaultBufferSize),
		closeChannel:            make(chan struct{}),
		streamClosedChannel:     make(chan struct{}),
		numReplicas:             numReplicas,
		shutdownWG:              shutdownWG,
		forceUnloadCh:           make(chan struct{}),
		limitsEnabled:           limitsEnabled,
		maxSequenceNumber:       common.GetRandInt64(int64(extentRolloverSeqnumMin), int64(extentRolloverSeqnumMax)),
		extMetrics:              load.NewExtentMetrics(),
		dstMetrics:              pathCache.dstMetrics,
		hostMetrics:             pathCache.hostMetrics,
		lastExtLoadReportedTime: time.Now().UnixNano(),
	}
	if pathCache.destType == shared.DestinationType_LOG {
		conn.lastSuccessSeqNoCh = make(chan int64, 1)
	}
	conn.loadReporter = loadReporterFactory.CreateReporter(extLoadReportingInterval, conn, logger)

	// set minimumAllowedMessageDelaySeconds for timer-destinations
	if conn.destType == shared.DestinationType_TIMER {
		if strings.HasPrefix(pathCache.destinationPath, "/test") { // override min delay for tests
			conn.minimumAllowedMessageDelaySeconds = minimumAllowedMessageDelaySecondsTest
		} else {
			conn.minimumAllowedMessageDelaySeconds = minimumAllowedMessageDelaySeconds
		}
	}

	// Initialize the token bucket
	conn.SetMsgsLimitPerSecond(common.HostPerExtentMsgsLimitPerSecond)
	return conn
}

// setReplicaInfo sets the replica info for this hostport by setting the connection and
// creating a timer for this replica object
func (conn *extHost) setReplicaInfo(hostport storeHostPort, replicaConn *replicaConnection) {
	conn.streams[hostport] = &replicaInfo{
		conn:      replicaConn,
		sendTimer: common.NewTimer(replicaSendTimeout),
	}
}

func (conn *extHost) open() {
	conn.lk.Lock()
	defer conn.lk.Unlock()

	if !conn.opened {
		conn.loadReporter.Start()
		conn.waitWriteWG.Add(1)
		go conn.writeMessagesPump()
		conn.waitReadWG.Add(1)
		go conn.aggregateAndSendReplies(conn.numReplicas)
		conn.opened = true

		conn.logger.WithField(`extentRolloverSeqnum`, conn.maxSequenceNumber).Info("extHost opened")
	}
}

func (conn *extHost) shutdown() {
	// make sure we don't wait for the unloadTimeout
	close(conn.forceUnloadCh)
	conn.close()
}

func (conn *extHost) close() {

	conn.lk.Lock()
	if conn.closed {
		conn.lk.Unlock()
		return
	}

	conn.closed = true

	// Shutdown order:
	// 1. stop the write pump to replicas and wait for the pump to close
	// 2. close the replica streams
	// 3. stop the read pump from replicas
	close(conn.closeChannel)
	if ok := common.AwaitWaitGroup(&conn.waitWriteWG, defaultWGTimeout); !ok {
		conn.logger.Fatal("waitWriteGroup timed out")
	}
	for _, stream := range conn.streams {
		stream.conn.close()
		// stop the timer as well so that it gets gc'ed
		stream.sendTimer.Stop()
		// release the client, which will inturn close the channel
		conn.tClients.ReleaseThriftStoreClient(conn.destUUID)
	}
	close(conn.streamClosedChannel)
	close(conn.replyClientCh)
	if conn.lastSuccessSeqNoCh != nil {
	CLOSED:
		for {
			select {
			case _, ok := <-conn.lastSuccessSeqNoCh:
				if !ok {
					break CLOSED
				}
			}
		}
	}

	if ok := common.AwaitWaitGroup(&conn.waitReadWG, defaultWGTimeout); !ok {
		conn.logger.Fatal("waitReadGroup timed out")
	}
	// we are not going to resuse the extents at this point
	// seal the extent
	if err := conn.sealExtent(); err != nil {
		conn.logger.Warn("seal extent notify failed during closed")
	}

	conn.lk.Unlock() // no longer need the lock

	conn.logger.WithFields(bark.Fields{
		`sentSeqNo`: conn.seqNo,
		`ackSeqNo`:  conn.lastSuccessSeqNo,
	}).Info("extHost closed")

	// notify the pathCache so that we can tear down the client
	// connections if needed
	conn.notifyExtCacheClosedCh <- conn.extUUID

	unloadTimer := common.NewTimer(unloadTimeout)
	defer unloadTimer.Stop()
	// now wait for unload timeout to keep the extent loaded in the pathCache
	// this is needed to deal with the eventually consistent nature of cassandra.
	// After an extent is marked as SEALED, a subsequent listDestinationExtents
	// might still continue to show the extent as OPENED. To avoid agressive
	// unload/reload (store would reject the call to openStream), sleep for
	// a while before totally unloading
	// or wait for the force shutdown which will happen when we are completely unloading
	// the pathCache
	select {
	case <-unloadTimer.C:
	case <-conn.forceUnloadCh:
	}

	// now notify the pathCache to unload the extent
	conn.notifyExtCacheUnloadCh <- conn.extUUID

	conn.loadReporter.Stop()
	conn.shutdownWG.Done()
}

func (conn *extHost) getEnqueueTime() int64 {
	enqueueTime := time.Now().UnixNano()

	// ensure the enqueue-time never rolls back
	if enqueueTime >= conn.lastEnqueueTime {
		conn.lastEnqueueTime = enqueueTime
	} else {
		conn.logger.WithField("context", fmt.Sprintf("enqueueTime=%x < conn.lastEnqueueTime=%x", enqueueTime, conn.lastEnqueueTime)).
			Warn("inputhost: current time less than last enqueue-time")

		enqueueTime = conn.lastEnqueueTime
	}
	return enqueueTime
}

// sendMessageToReplicas is the place where we serialize the messages to be sent to the replicas.
// XXX: Care must be taken to ensure we don't call this routine in parallel without proper synchronization
// When invoked with nil as parameter values sends message with FullyReplicatedWatermark only
func (conn *extHost) sendMessageToReplicas(pr *inPutMessage, extSendTimer *common.Timer, watermark *int64) (int64, error) {
	var err error

	watermarkOnly := pr == nil
	if watermarkOnly && conn.destType != shared.DestinationType_LOG {
		log.Fatal("WatermarkOnly message requested for non LOG destination")
	}
	msg := store.NewAppendMessage()
	var appendMsgAckCh chan *store.AppendMessageAck
	if watermarkOnly {
		if watermark == nil {
			log.Fatal("nil watermark and pr")
		}
		if conn.lastSentWatermark == *watermark {
			return -1, nil
		}
	}

	conn.extMetrics.Increment(load.ExtentMetricMsgsIn)
	conn.dstMetrics.Increment(load.DstMetricMsgsIn)
	conn.hostMetrics.Increment(load.HostMetricMsgsIn)

	// increment seq-num; do atomically, since this could
	// be concurrently queried by the reporter
	sequenceNumber := atomic.AddInt64(&conn.seqNo, 1)
	msg.SequenceNumber = common.Int64Ptr(sequenceNumber)
	msg.EnqueueTimeUtc = common.Int64Ptr(conn.getEnqueueTime())
	if !watermarkOnly {
		msg.Payload = pr.putMsg
		appendMsgAckCh = make(chan *store.AppendMessageAck, 5)
	}
	if watermark != nil && conn.lastSentWatermark < *watermark {
		msg.FullyReplicatedWatermark = watermark
	}

	// we write the above same message to all the replicas
	// even if one of the replicas fail, we consider the message failed
	// no need to lock the conn.streams here because the replica set
	// for an extent will not change at all
	errCh := make(chan error)
	for _, stream := range conn.streams {
		go func(replInfo *replicaInfo, aMsg *store.AppendMessage, aMsgAckCh chan *store.AppendMessageAck) {
			pMsg := &replicaPutMsg{
				appendMsg:      aMsg,
				appendMsgAckCh: aMsgAckCh,
			}

			// log disabled due to CPU cost
			// conn.logger.WithFields(logger.Fields{`replica`: replica,  common.TagSeq: conn.seqNo,  `Payload`: msg.Payload,}).Debug(`inputhost: sending data to store: ; seqno: , data`)

			replInfo.sendTimer.Reset(replicaSendTimeout)
			select {
			case replInfo.conn.putMessagesCh <- pMsg:
			case <-replInfo.sendTimer.C:
				errCh <- ErrTimeout
				return
			}
			errCh <- nil
			return
		}(stream, msg, appendMsgAckCh)
	}
	// Wait for all the go routines above; we wait on the errCh to get the response from all replicas
	for replica, stream := range conn.streams {
		err = <-errCh
		if err != nil {
			if watermarkOnly {
				conn.logger.WithFields(bark.Fields{`replica`: replica, common.TagErr: err, `putMessagesChLength`: len(stream.conn.putMessagesCh)}).Warn(`inputhost: sending fully replicated watermark to replica: , failed with error: ; length of putMsgCh: ;`)
			} else {
				conn.logger.WithFields(bark.Fields{`replica`: replica, common.TagErr: err, `putMessagesChLength`: len(stream.conn.putMessagesCh), `replyChLength`: len(stream.conn.replyCh)}).Error(`inputhost: sending msg to replica: , failed with error: ; length of putMsgCh: ; length of replyCh: ;`)
			}
			return sequenceNumber, err
		}
	}

	if !watermarkOnly {
		extSendTimer.Reset(replicaSendTimeout)
		// this is for the extHost's inflight messages for a successful message
		select {
		case conn.replyClientCh <- writeResponse{pr.putMsg.GetID(), sequenceNumber, appendMsgAckCh, pr.putMsgAckCh, pr.putMsgRecvTime, pr.putMsg.GetUserContext()}:
		case <-extSendTimer.C:
			conn.logger.WithField(`lenReplyClientCh`, len(conn.replyClientCh)).Error(`inputhost: exthost: sending msg to the replyClientCh on exthost timed out`)
			err = ErrTimeout
		}
	}
	if err == nil && watermark != nil {
		conn.lastSentWatermark = *watermark
	}
	return sequenceNumber, err
}

func (conn *extHost) writeMessagesPump() {
	defer conn.waitWriteWG.Done()
	// Setup the extIdleTimer
	extIdleTimer := common.NewTimer(extIdleTimeout)
	defer extIdleTimer.Stop()

	// setup extSendTimer which is the timer for the intermediate extHost's channel
	extSendTimer := common.NewTimer(replicaSendTimeout)
	defer extSendTimer.Stop()

	var watermark *int64

	for {
		// reset the idle timer
		extIdleTimer.Reset(extIdleTimeout)
		select {
		case pr, ok := <-conn.putMessagesCh:
			if !ok {
				conn.logger.Error("inputhost: extHost: put message ch closed")
				return
			}
			if pr == nil {
				conn.logger.Fatal("Nil put message")
			}
			conn.sendMessage(pr, extSendTimer, watermark)
		case <-extIdleTimer.C:
			// first try to seal, if it succeeeds then close it
			if err := conn.sealExtent(); err == nil {
				conn.logger.WithField(`extIdleTimeout`, extIdleTimeout).Debug(`extent idle for: seconds and seal notified to extent controller; closing it`)
				go conn.close()
				return
			}
		case w := <-conn.lastSuccessSeqNoCh: // Never selected if channel is nil
			// Get the last sequence number from the channel
		OUT:
			for {
				select {
				case w = <-conn.lastSuccessSeqNoCh:
				default:
					break OUT
				}
			}
			watermark = common.Int64Ptr(w)
			var pr *inPutMessage
			select {
			case pr = <-conn.putMessagesCh:
			default:
			}
			if pr != nil {
				// piggyback on the next message
				conn.sendMessage(pr, extSendTimer, watermark)
			} else {
				// send just watermark as there is no message to piggyback on
				_, err := conn.sendMessageToReplicas(nil, nil, watermark)

				if err != nil {
					conn.logger.WithField(common.TagErr, err).Error(`inputhost: extHost: failure sending watermark`)
					go conn.close()
					return
				}
			}
			watermark = nil
		case <-conn.closeChannel:
			return
		}
	}
}

func (conn *extHost) sendMessage(pr *inPutMessage, extSendTimer *common.Timer, watermark *int64) {
	// make sure we can satisfy the rate, if needed
	if conn.limitsEnabled {
		if ok, _ := conn.GetExtTokenBucketValue().TryConsume(1); !ok {
			// we couldn't acquire the token. just return throttled error here
			conn.logger.
				WithField(common.TagInPutAckID, common.FmtInPutAckID(pr.putMsg.GetID())).
				Warn("inputhost: extHost: rate exceeded. throttling the message")
			// Immediately send throttled status back to the client so that
			// the client can throttle
			pr.putMsgAckCh <- &cherami.PutMessageAck{
				ID:          common.StringPtr(pr.putMsg.GetID()),
				UserContext: pr.putMsg.GetUserContext(),
				Status:      common.CheramiStatusPtr(cherami.Status_THROTTLED),
				Message:     common.StringPtr("throttling: inputhost rate exceeded"),
			}
			return
		}
	}

	// for timer-queues, ensure that the delay in the message is not less than the
	// minimum allowed delay. we use a minimum delay to ensure that time-skews (upto
	// the minimumAllowedMessageDelaySeconds) between inputhost and storehost do not
	// result in data loss, due to messages being inserted into the "past".
	if conn.destType == shared.DestinationType_TIMER &&
		pr.putMsg.GetDelayMessageInSeconds() < conn.minimumAllowedMessageDelaySeconds {

		conn.logger.
			WithField(common.TagInPutAckID, common.FmtInPutAckID(pr.putMsg.GetID())).
			Warn("inputhost: extHost: message delay exceeds minimum allowed; rejecting message")

		// n-ack message, since it exceeds minimum allowed delay
		pr.putMsgAckCh <- &cherami.PutMessageAck{
			ID:          common.StringPtr(pr.putMsg.GetID()),
			UserContext: pr.putMsg.GetUserContext(),
			Status:      common.CheramiStatusPtr(cherami.Status_FAILED),
			Message:     common.StringPtr("delay exceeds minimum allowed"),
		}

		return
	}

	sequenceNumber, err := conn.sendMessageToReplicas(pr, extSendTimer, watermark)
	if err != nil {
		// For now, lets reply Status_FAILED immediately and
		// close the connection if we got an error.
		// this will result in the creation of a new extent, probably.
		pr.putMsgAckCh <- &cherami.PutMessageAck{
			ID:          common.StringPtr(pr.putMsg.GetID()),
			UserContext: pr.putMsg.GetUserContext(),
			Status:      common.CheramiStatusPtr(cherami.Status_FAILED),
			Message:     common.StringPtr(err.Error()),
		}
		go conn.close()
		return
	}

	// If we reach the max sequence number, notify the extent controller but
	// keep the pumps open
	// Eventually we will get an error from the store when the extent is sealed
	// notify only if we have not already sent the notification
	if sequenceNumber >= conn.maxSequenceNumber && atomic.LoadUint32(&conn.sealed) == open {
		// notify asynchronously
		go conn.sealExtent()
	}
}

func (conn *extHost) aggregateAndSendReplies(numReplicas int) {
	inflightMessages := make(map[int64]writeResponse)
	defer conn.failInflightMessages(inflightMessages)

	// Setup the perMsgTimer
	perMsgTimer := common.NewTimer(msgAckTimeout)
	defer perMsgTimer.Stop()

	if conn.lastSuccessSeqNoCh != nil {
		defer close(conn.lastSuccessSeqNoCh)
	}

	for {
		select {
		case resCh, ok := <-conn.replyClientCh: // resCh is a writeResponse; each replica has a copy of this structure that it will use to send us ACK's through the appendMsgAck channel
			if ok {
				inflightMessages[resCh.seqNo] = resCh

				var stat cherami.Status
				var address int64 // from storage's appendMsgAck. Should be the same across all replicas

				// this is where we wait for all the replicas to reply.

				// make sure we reset the timer properly based on the sentTime
				elapsed := time.Since(resCh.sentTime)

				// Note: even if this value is negative, it is ok because we should timeout immediately
				perMsgTimer.Reset(msgAckTimeout - elapsed)
				for i := 0; i < numReplicas; i++ {
					select {
					case ack, ok := <-resCh.appendMsgAck:
						if !ok || ack.GetStatus() != cherami.Status_OK {
							stat = ack.GetStatus()
							// error means we shutdown this extent and seal it
							go conn.close()
						}

						if address == 0 && ok && ack.GetStatus() == cherami.Status_OK {
							address = ack.GetAddress()
						}
					case <-perMsgTimer.C:
						conn.logger.Error("timed out waiting for ack from replica")
						stat = cherami.Status_FAILED
						go conn.close()
					case <-conn.streamClosedChannel:
						// all streams are closed.. no point waiting for acks
						return
					}
				}
				// mark this seqNo as the last success seqno
				conn.lastSuccessSeqNo = resCh.seqNo
				// Notify about the last seqNo removing the previous notification as it is not needed anymore
				// Without such removal channel buffer would need to be larger than the maximum number of
				// outstanding notifications to avoid deadlocks.
				if conn.lastSuccessSeqNoCh != nil {
				PUSHED:
					for {
						select {
						case conn.lastSuccessSeqNoCh <- resCh.seqNo:
							break PUSHED
						default:
							select {
							case <-conn.lastSuccessSeqNoCh:
							default:
							}
						}
					}
				}
				// Now send the reply back to the pubConnection and ultimately on the stream to the publisher
				putMsgAck := cherami.NewPutMessageAck()
				putMsgAck.ID = common.StringPtr(resCh.ackID)
				putMsgAck.UserContext = resCh.userContext
				putMsgAck.Status = common.CheramiStatusPtr(stat)
				putMsgAck.Receipt = common.StringPtr(
					fmt.Sprintf("%s:%d:%8x", string(conn.extUUID), resCh.seqNo, address))

				// Try to send the ack back to the client within the timeout period
				perMsgTimer.Reset(msgAckTimeout)
				select {
				case resCh.putMsgAck <- putMsgAck:
				case <-perMsgTimer.C:
					conn.logger.WithField(common.TagAckID, resCh.ackID).Error(`sending ack back to the client timed out`)
				}
				delete(inflightMessages, resCh.seqNo)
			} else {
				// we are closing the connection.
				return
			}
		case <-conn.streamClosedChannel:
			return
		}
	}
}

// sealExtent calls the extents unreachable error on the given extent
// and seals the extent at the lastSuccessSeqNumber
func (conn *extHost) sealExtent() error {
	conn.sealLk.Lock()
	defer conn.sealLk.Unlock()

	if atomic.LoadUint32(&conn.sealed) == sealed {
		// already sealed
		return nil
	}
	// we have not sealed yet, so notify the extent controller
	extController, err := conn.tClients.GetAdminControllerClient()
	if err == nil {
		update := &admin.ExtentUnreachableNotification{
			DestinationUUID:    common.StringPtr(conn.destUUID),
			ExtentUUID:         common.StringPtr(conn.extUUID),
			SealSequenceNumber: common.Int64Ptr(conn.lastSuccessSeqNo),
		}

		req := &admin.ExtentsUnreachableRequest{
			UpdateUUID: common.StringPtr(uuid.New()),
			Updates:    []*admin.ExtentUnreachableNotification{update},
		}

		conn.logger.WithField(common.TagSeq, conn.lastSuccessSeqNo).Info("Notifying controller to seal extent")

		ctx, cancel := thrift.NewContext(thriftCallTimeout)
		defer cancel()
		// TODO: add retry here.
		if err := extController.ExtentsUnreachable(ctx, req); err != nil {
			conn.logger.WithField(common.TagErr, err).Error(`ExtentsUnreachable call failed with err`)
			return err
		}

		// if we are here, we have notified the extent controller. mark that we have already sealed
		atomic.StoreUint32(&conn.sealed, sealed)
		return nil
	}

	return err
}
func (conn *extHost) failInflightMessages(inflightMessages map[int64]writeResponse) {
	defer conn.waitReadWG.Done()

	// First drain the replyCh to get all inflight messages which are in the
	// buffer
DrainLoop:
	for {
		select {
		case resCh, ok := <-conn.replyClientCh:
			if !ok {
				break DrainLoop
			}
			inflightMessages[resCh.seqNo] = resCh
		default:
			break DrainLoop
		}
	}

	for _, respCh := range inflightMessages {
		putMsgAck := &cherami.PutMessageAck{
			ID:          common.StringPtr(respCh.ackID),
			UserContext: respCh.userContext,
			Status:      common.CheramiStatusPtr(cherami.Status_FAILED),
			Message:     common.StringPtr("closing down extent"),
		}
		// It is ok to do a non-blocking send here during shutdown because we will
		// be failing all messages anyway..
		select {
		case respCh.putMsgAck <- putMsgAck:
		case <-conn.forceUnloadCh:
		}
	}
}

// Report is used for reporting Destination Extent specific load to controller
func (conn *extHost) Report(reporter common.LoadReporter) {
	// TODO: Report Extent specific load like incomingMessageCount, incomingBytesCount, putLatency
	now := time.Now().UnixNano()
	intervalSecs := (now - conn.lastExtLoadReportedTime) / int64(time.Second)
	if intervalSecs < 1 {
		return
	}

	msgsInPerSec := conn.extMetrics.GetAndReset(load.ExtentMetricMsgsIn) / intervalSecs

	metric := controller.DestinationExtentMetrics{
		IncomingMessagesCounter: common.Int64Ptr(msgsInPerSec),
	}
	reporter.ReportDestinationExtentMetric(conn.destUUID, conn.extUUID, metric)
}

// GetMsgsLimitPerSecond gets per extent rate limit for this extent
func (conn *extHost) GetMsgsLimitPerSecond() int {
	return int(atomic.LoadInt32(&conn.extentMsgsLimitPerSecond))
}

// SetMsgsLimitPerSecond sets per extent rate limit for this extent
func (conn *extHost) SetMsgsLimitPerSecond(connLimit int32) {
	atomic.StoreInt32(&conn.extentMsgsLimitPerSecond, connLimit)
	conn.SetExtTokenBucketValue(int32(connLimit))
}

// GetExtTokenBucketValue gets token bucket for extentMsgsLimitPerSecond
func (conn *extHost) GetExtTokenBucketValue() common.TokenBucket {
	return conn.extTokenBucketValue.Load().(common.TokenBucket)
}

// SetExtTokenBucketValue sets token bucket for extentMsgsLimitPerSecond
func (conn *extHost) SetExtTokenBucketValue(connLimit int32) {
	tokenBucket := common.NewTokenBucket(int(connLimit), common.NewRealTimeSource())
	conn.extTokenBucketValue.Store(tokenBucket)
}
