/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package proxy

import (
	"container/list"
	"context"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"sync/atomic"
	"time"

	v2 "sofastack.io/sofa-mosn/pkg/api/v2"
	"sofastack.io/sofa-mosn/pkg/trace"
	"sofastack.io/sofa-mosn/pkg/utils"

	"runtime/debug"

	"sofastack.io/sofa-mosn/pkg/buffer"
	"sofastack.io/sofa-mosn/pkg/log"
	"sofastack.io/sofa-mosn/pkg/protocol"
	"sofastack.io/sofa-mosn/pkg/protocol/http"
	"sofastack.io/sofa-mosn/pkg/router"
	"sofastack.io/sofa-mosn/pkg/types"

	mosnctx "sofastack.io/sofa-mosn/pkg/context"
)

// types.StreamEventListener
// types.StreamReceiveListener
// types.FilterChainFactoryCallbacks
// Downstream stream, as a controller to handle downstream and upstream proxy flow
type downStream struct {
	ID      uint32
	proxy   *proxy
	route   types.Route
	cluster types.ClusterInfo
	element *list.Element

	// flow control
	bufferLimit uint32

	// ~~~ control args
	timeout    Timeout
	retryState *retryState

	requestInfo     types.RequestInfo
	responseSender  types.StreamSender
	upstreamRequest *upstreamRequest
	perRetryTimer   *utils.Timer
	responseTimer   *utils.Timer

	// ~~~ downstream request buf
	downstreamReqHeaders  types.HeaderMap
	downstreamReqDataBuf  types.IoBuffer
	downstreamReqTrailers types.HeaderMap

	// ~~~ downstream response buf
	downstreamRespHeaders  types.HeaderMap
	downstreamRespDataBuf  types.IoBuffer
	downstreamRespTrailers types.HeaderMap

	// ~~~ state
	// starts to send back downstream response, set on upstream response detected
	downstreamResponseStarted bool
	// downstream request received done
	downstreamRecvDone bool
	// upstream req sent
	upstreamRequestSent bool
	// 1. at the end of upstream response 2. by a upstream reset due to exceptions, such as no healthy upstream, connection close, etc.
	upstreamProcessDone bool
	// don't convert headers, data and trailers.  e.g. activeStreamReceiverFilter.Appendxx
	noConvert bool
	// direct response.  e.g. sendHijack
	directResponse bool
	// oneway
	oneway bool

	notify chan struct{}

	downstreamReset   uint32
	downstreamCleaned uint32
	upstreamReset     uint32
	reuseBuffer       uint32

	resetReason types.StreamResetReason

	//filters
	senderFilters        []*activeStreamSenderFilter
	senderFiltersIndex   int
	receiverFilters      []*activeStreamReceiverFilter
	receiverFiltersIndex int
	receiverFiltersAgain bool

	context context.Context

	// stream access logs
	streamAccessLogs []types.AccessLog
	logDone          uint32

	snapshot types.ClusterSnapshot
}

func newActiveStream(ctx context.Context, proxy *proxy, responseSender types.StreamSender, span types.Span) *downStream {
	if span != nil && trace.IsTracingEnabled() {
		ctx = mosnctx.WithValue(ctx, types.ContextKeyActiveSpan, span)
		ctx = mosnctx.WithValue(ctx, types.ContextKeyTraceSpanKey, &trace.SpanKey{TraceId: span.TraceId(), SpanId: span.SpanId()})
	}

	proxyBuffers := proxyBuffersByContext(ctx)

	stream := &proxyBuffers.stream
	stream.ID = atomic.AddUint32(&currProxyID, 1)
	stream.proxy = proxy
	stream.requestInfo = &proxyBuffers.info
	stream.requestInfo.SetStartTime()
	stream.context = ctx
	stream.reuseBuffer = 1
	stream.notify = make(chan struct{}, 1)

	if responseSender == nil || reflect.ValueOf(responseSender).IsNil() {
		stream.oneway = true
	} else {
		stream.responseSender = responseSender
		stream.responseSender.GetStream().AddEventListener(stream)
	}

	proxy.stats.DownstreamRequestTotal.Inc(1)
	proxy.stats.DownstreamRequestActive.Inc(1)
	proxy.listenerStats.DownstreamRequestTotal.Inc(1)
	proxy.listenerStats.DownstreamRequestActive.Inc(1)

	// info message for new downstream
	if log.Proxy.GetLogLevel() >= log.INFO {
		requestId := mosnctx.Get(stream.context, types.ContextKeyStreamID)
		log.Proxy.Infof(stream.context, "[proxy] [downstream] new stream, proxyId = %d , requestId =%v, oneway=%t", stream.ID, requestId, stream.oneway)
	}
	return stream
}

// downstream's lifecycle ends normally
func (s *downStream) endStream() {
	if s.responseSender != nil && !s.downstreamRecvDone {
		// not reuse buffer
		atomic.StoreUint32(&s.reuseBuffer, 0)
	}
	s.cleanStream()

	// note: if proxy logic resets the stream, there maybe some underlying data in the conn.
	// we ignore this for now, fix as a todo
}

// Clean up on the very end of the stream: end stream or reset stream
// Resources to clean up / reset:
// 	+ upstream request
// 	+ all timers
// 	+ all filters
//  + remove stream in proxy context
func (s *downStream) cleanStream() {
	if !atomic.CompareAndSwapUint32(&s.downstreamCleaned, 0, 1) {
		return
	}

	s.requestInfo.SetRequestFinishedDuration(time.Now())

	streamDurationNs := s.requestInfo.RequestFinishedDuration().Nanoseconds()

	// reset corresponding upstream stream
	if s.upstreamRequest != nil && !s.upstreamProcessDone && !s.oneway {
		log.Proxy.Errorf(s.context, "[proxy] [downstream] upstreamRequest.resetStream, proxyId: %d", s.ID)
		s.upstreamProcessDone = true
		s.upstreamRequest.resetStream()
	}

	// clean up timers
	s.cleanUp()

	// tell filters it's time to destroy
	for _, ef := range s.senderFilters {
		ef.filter.OnDestroy()
	}

	for _, ef := range s.receiverFilters {
		ef.filter.OnDestroy()
	}

	// countdown metrics
	s.proxy.stats.DownstreamRequestActive.Dec(1)
	s.proxy.stats.DownstreamRequestTime.Update(streamDurationNs)
	s.proxy.stats.DownstreamRequestTimeTotal.Inc(streamDurationNs)

	s.proxy.listenerStats.DownstreamRequestActive.Dec(1)
	s.proxy.listenerStats.DownstreamRequestTime.Update(streamDurationNs)
	s.proxy.listenerStats.DownstreamRequestTimeTotal.Inc(streamDurationNs)

	// finish tracing
	s.finishTracing()

	// write access log
	s.writeLog()

	// delete stream
	s.proxy.deleteActiveStream(s)

	// recycle if no reset events
	s.giveStream()
}

func (s *downStream) writeLog() {
	defer func() {
		if r := recover(); r != nil {
			log.Proxy.Errorf(s.context, "[proxy] [downstream] writeLog panic %v, downstream %+v", r, s)
		}
	}()

	if !atomic.CompareAndSwapUint32(&s.logDone, 0, 1) {
		return
	}
	// proxy access log
	if s.proxy != nil && s.proxy.accessLogs != nil {
		for _, al := range s.proxy.accessLogs {
			al.Log(s.downstreamReqHeaders, s.downstreamRespHeaders, s.requestInfo)
		}
	}

	// per-stream access log
	if s.streamAccessLogs != nil {
		for _, al := range s.streamAccessLogs {
			al.Log(s.downstreamReqHeaders, s.downstreamRespHeaders, s.requestInfo)
		}
	}
}

// types.StreamEventListener
// Called by stream layer normally
func (s *downStream) OnResetStream(reason types.StreamResetReason) {
	if !atomic.CompareAndSwapUint32(&s.downstreamReset, 0, 1) {
		return
	}

	s.resetReason = reason

	s.sendNotify()
}

func (s *downStream) ResetStream(reason types.StreamResetReason) {
	s.proxy.stats.DownstreamRequestReset.Inc(1)
	s.proxy.listenerStats.DownstreamRequestReset.Inc(1)
	s.cleanStream()
}

func (s *downStream) OnDestroyStream() {}

// types.StreamReceiveListener
func (s *downStream) OnReceive(ctx context.Context, headers types.HeaderMap, data types.IoBuffer, trailers types.HeaderMap) {
	s.downstreamReqHeaders = headers
	if data != nil {
		s.downstreamReqDataBuf = data.Clone()
		data.Drain(data.Len())
	}
	s.downstreamReqTrailers = trailers

	if log.Proxy.GetLogLevel() >= log.DEBUG {
		log.Proxy.Debugf(s.context, "[proxy] [downstream] OnReceive headers:%+v, data:%+v, trailers:%+v", headers, data, trailers)
	}

	id := s.ID
	// goroutine for proxy
	pool.ScheduleAuto(func() {
		defer func() {
			if r := recover(); r != nil {
				log.Proxy.Errorf(s.context, "[proxy] [downstream] OnReceive panic: %v, downstream: %+v, oldId: %d, newId: %d\n%s",
					r, s, id, s.ID, string(debug.Stack()))

				if id == s.ID {
					s.writeLog()
				}
			}
		}()

		phase := types.InitPhase
		for i := 0; i < 5; i++ {
			s.cleanNotify()

			phase = s.receive(ctx, id, phase)
			switch phase {
			case types.End:
				return
			case types.MatchRoute:
				log.Proxy.Debugf(s.context, "[proxy] [downstream] redo match route %+v", s)
			case types.Retry:
				log.Proxy.Debugf(s.context, "[proxy] [downstream] retry %+v", s)
			case types.UpFilter:
				log.Proxy.Debugf(s.context, "[proxy] [downstream] directResponse %+v", s)
			}
		}
	})
}

func (s *downStream) receive(ctx context.Context, id uint32, phase types.Phase) types.Phase {
	for i := 0; i <= int(types.End-types.InitPhase); i++ {
		switch phase {
		// init phase
		case types.InitPhase:
			phase++

			// downstream filter before route
		case types.DownFilter:
			if log.Proxy.GetLogLevel() >= log.DEBUG {
				log.Proxy.Debugf(s.context, "[proxy] [downstream] enter phase %d, proxyId = %d  ", phase, id)
			}
			s.runReceiveFilters(phase, s.downstreamReqHeaders, s.downstreamReqDataBuf, s.downstreamReqTrailers)

			if p, err := s.processError(id); err != nil {
				return p
			}
			phase++

			// match route
		case types.MatchRoute:
			if log.Proxy.GetLogLevel() >= log.DEBUG {
				log.Proxy.Debugf(s.context, "[proxy] [downstream] enter phase %d, proxyId = %d  ", phase, id)
			}
			s.matchRoute()
			if p, err := s.processError(id); err != nil {
				return p
			}
			phase++

			// downstream filter after route
		case types.DownFilterAfterRoute:
			if log.Proxy.GetLogLevel() >= log.DEBUG {
				log.Proxy.Debugf(s.context, "[proxy] [downstream] enter phase %d, proxyId = %d  ", phase, id)
			}
			s.runReceiveFilters(phase, s.downstreamReqHeaders, s.downstreamReqDataBuf, s.downstreamReqTrailers)

			if p, err := s.processError(id); err != nil {
				return p
			}
			phase++

			// downstream receive header
		case types.DownRecvHeader:
			if s.downstreamReqHeaders != nil {
				if log.Proxy.GetLogLevel() >= log.DEBUG {
					log.Proxy.Debugf(s.context, "[proxy] [downstream] enter phase %d, proxyId = %d  ", phase, id)
				}
				s.receiveHeaders(s.downstreamReqDataBuf == nil && s.downstreamReqTrailers == nil)

				if p, err := s.processError(id); err != nil {
					return p
				}
			}
			phase++

			// downstream receive data
		case types.DownRecvData:
			if s.downstreamReqDataBuf != nil {
				if log.Proxy.GetLogLevel() >= log.DEBUG {
					log.Proxy.Debugf(s.context, "[proxy] [downstream] enter phase %d, proxyId = %d  ", phase, id)
				}
				s.downstreamReqDataBuf.Count(1)
				s.receiveData(s.downstreamReqTrailers == nil)

				if p, err := s.processError(id); err != nil {
					return p
				}
			}
			phase++

			// downstream receive trailer
		case types.DownRecvTrailer:
			if s.downstreamReqTrailers != nil {
				if log.Proxy.GetLogLevel() >= log.DEBUG {
					log.Proxy.Debugf(s.context, "[proxy] [downstream] enter phase %d, proxyId = %d  ", phase, id)
				}
				s.receiveTrailers()

				if p, err := s.processError(id); err != nil {
					return p
				}
			}
			phase++

			// downstream oneway
		case types.Oneway:
			if s.oneway {
				if log.Proxy.GetLogLevel() >= log.DEBUG {
					log.Proxy.Debugf(s.context, "[proxy] [downstream] enter phase %d, proxyId = %d  ", phase, id)
				}
				s.cleanStream()

				// downstreamCleaned has set, return types.End
				if p, err := s.processError(id); err != nil {
					return p
				}
			}

			// no oneway, skip types.Retry
			phase = types.WaitNofity

			// retry request
		case types.Retry:
			if log.Proxy.GetLogLevel() >= log.DEBUG {
				log.Proxy.Debugf(s.context, "[proxy] [downstream] enter phase %d, proxyId = %d  ", phase, id)
			}

			if s.downstreamReqDataBuf != nil {
				s.downstreamReqDataBuf.Count(1)
			}
			s.doRetry()
			if p, err := s.processError(id); err != nil {
				return p
			}
			phase++

			// wait for upstreamRequest or reset
		case types.WaitNofity:
			if log.Proxy.GetLogLevel() >= log.DEBUG {
				log.Proxy.Debugf(s.context, "[proxy] [downstream] enter phase %d, proxyId = %d  ", phase, id)
			}
			if p, err := s.waitNotify(id); err != nil {
				return p
			}

			if log.Proxy.GetLogLevel() >= log.DEBUG {
				log.Proxy.Debugf(s.context, "[proxy] [downstream] OnReceive send downstream response %+v", s.downstreamRespHeaders)
			}

			phase++

			// upstream filter
		case types.UpFilter:
			if log.Proxy.GetLogLevel() >= log.DEBUG {
				log.Proxy.Debugf(s.context, "[proxy] [downstream] enter phase %d, proxyId = %d  ", phase, id)
			}
			s.runAppendFilters(phase, s.downstreamRespHeaders, s.downstreamRespDataBuf, s.downstreamRespTrailers)

			if p, err := s.processError(id); err != nil {
				return p
			}

			// maybe direct response
			if s.upstreamRequest == nil {
				fakeUpstreamRequest := &upstreamRequest{
					downStream: s,
				}

				s.upstreamRequest = fakeUpstreamRequest
			}

			phase++

			// upstream receive header
		case types.UpRecvHeader:
			// send downstream response
			if s.downstreamRespHeaders != nil {
				if log.Proxy.GetLogLevel() >= log.DEBUG {
					log.Proxy.Debugf(s.context, "[proxy] [downstream] enter phase %d, proxyId = %d  ", phase, id)
				}
				s.upstreamRequest.receiveHeaders(s.downstreamRespDataBuf == nil && s.downstreamRespTrailers == nil)

				if p, err := s.processError(id); err != nil {
					return p
				}
			}
			phase++

			// upstream receive data
		case types.UpRecvData:
			if s.downstreamRespDataBuf != nil {
				if log.Proxy.GetLogLevel() >= log.DEBUG {
					log.Proxy.Debugf(s.context, "[proxy] [downstream] enter phase %d, proxyId = %d  ", phase, id)
				}
				s.upstreamRequest.receiveData(s.downstreamRespTrailers == nil)

				if p, err := s.processError(id); err != nil {
					return p
				}
			}
			phase++

			// upstream receive triler
		case types.UpRecvTrailer:
			if s.downstreamRespTrailers != nil {
				if log.Proxy.GetLogLevel() >= log.DEBUG {
					log.Proxy.Debugf(s.context, "[proxy] [downstream] enter phase %d, proxyId = %d  ", phase, id)
				}
				s.upstreamRequest.receiveTrailers()

				if p, err := s.processError(id); err != nil {
					return p
				}
			}
			phase++

			// process end
		case types.End:
			return types.End

		default:
			log.Proxy.Errorf(s.context, "[proxy] [downstream] unexpected phase: %d", phase)
			return types.End
		}
	}

	log.Proxy.Errorf(s.context, "[proxy] [downstream] unexpected phase cycle time")
	return types.End
}

func (s *downStream) matchRoute() {
	headers := s.downstreamReqHeaders
	if s.proxy.routersWrapper == nil || s.proxy.routersWrapper.GetRouters() == nil {
		log.Proxy.Errorf(s.context, "[proxy] [downstream] routersWrapper or routers in routersWrapper is nil while trying to get router, headers= %v", headers)
		s.requestInfo.SetResponseFlag(types.NoRouteFound)
		s.sendHijackReply(types.RouterUnavailableCode, headers)
		return
	}

	// get router instance and do routing
	routers := s.proxy.routersWrapper.GetRouters()
	// do handler chain
	handlerChain := router.CallMakeHandlerChain(s.context, headers, routers, s.proxy.clusterManager)
	// handlerChain should never be nil
	if handlerChain == nil {
		log.Proxy.Errorf(s.context, "[proxy] [downstream] no route to make handler chain, headers = %v", headers)
		s.requestInfo.SetResponseFlag(types.NoRouteFound)
		s.sendHijackReply(types.RouterUnavailableCode, headers)
		return
	}
	s.snapshot, s.route = handlerChain.DoNextHandler()
}

func (s *downStream) convertProtocol() (dp, up types.Protocol) {
	dp = s.getDownstreamProtocol()
	up = s.getUpstreamProtocol()
	return
}

func (s *downStream) getDownstreamProtocol() (prot types.Protocol) {
	if s.proxy.serverStreamConn == nil {
		prot = types.Protocol(s.proxy.config.DownstreamProtocol)
	} else {
		prot = s.proxy.serverStreamConn.Protocol()
	}
	return prot
}

func (s *downStream) getUpstreamProtocol() (currentProtocol types.Protocol) {
	configProtocol := s.proxy.config.UpstreamProtocol

	// if route exists upstream protocol, it will replace the proxy config's upstream protocol
	if s.route != nil && s.route.RouteRule() != nil && s.route.RouteRule().UpstreamProtocol() != "" {
		configProtocol = s.route.RouteRule().UpstreamProtocol()
	}

	// Auto means same as downstream protocol
	if configProtocol == string(protocol.Auto) {
		currentProtocol = s.getDownstreamProtocol()
	} else {
		currentProtocol = types.Protocol(configProtocol)
	}

	return currentProtocol
}

func (s *downStream) receiveHeaders(endStream bool) {
	s.downstreamRecvDone = endStream

	// after stream filters run, check the route
	if s.route == nil {
		log.Proxy.Warnf(s.context, "[proxy] [downstream] no route to init upstream, headers = %v", s.downstreamReqHeaders)
		s.requestInfo.SetResponseFlag(types.NoRouteFound)
		s.sendHijackReply(types.RouterUnavailableCode, s.downstreamReqHeaders)
		return
	}
	// check if route have direct response
	// direct response will response now
	if resp := s.route.DirectResponseRule(); !(resp == nil || reflect.ValueOf(resp).IsNil()) {
		log.Proxy.Infof(s.context, "[proxy] [downstream] direct response, proxyId = %d", s.ID)
		if resp.Body() != "" {
			s.sendHijackReplyWithBody(resp.StatusCode(), s.downstreamReqHeaders, resp.Body())
		} else {
			s.sendHijackReply(resp.StatusCode(), s.downstreamReqHeaders)
		}
		return
	}
	// not direct response, needs a cluster snapshot and route rule
	if rule := s.route.RouteRule(); rule == nil || reflect.ValueOf(rule).IsNil() {
		log.Proxy.Warnf(s.context, "[proxy] [downstream] no route rule to init upstream, headers = %v", s.downstreamReqHeaders)
		s.requestInfo.SetResponseFlag(types.NoRouteFound)
		s.sendHijackReply(types.RouterUnavailableCode, s.downstreamReqHeaders)
		return
	}
	if s.snapshot == nil || reflect.ValueOf(s.snapshot).IsNil() {
		// no available cluster
		log.Proxy.Errorf(s.context, "[proxy] [downstream] cluster snapshot is nil, cluster name is: %s", s.route.RouteRule().ClusterName())
		s.requestInfo.SetResponseFlag(types.NoRouteFound)
		s.sendHijackReply(types.RouterUnavailableCode, s.downstreamReqHeaders)
		return
	}
	// as ClusterName has random factor when choosing weighted cluster,
	// so need determination at the first time
	clusterName := s.route.RouteRule().ClusterName()
	if log.Proxy.GetLogLevel() >= log.DEBUG {
		log.Proxy.Debugf(s.context, "[proxy] [downstream] route match result:%+v, clusterName=%v", s.route, clusterName)
	}

	s.cluster = s.snapshot.ClusterInfo()

	s.requestInfo.SetRouteEntry(s.route.RouteRule())
	s.requestInfo.SetDownstreamLocalAddress(s.proxy.readCallbacks.Connection().LocalAddr())
	// todo: detect remote addr
	s.requestInfo.SetDownstreamRemoteAddress(s.proxy.readCallbacks.Connection().RemoteAddr())

	pool, err := s.initializeUpstreamConnectionPool(s)
	if err != nil {
		log.Proxy.Errorf(s.context, "[proxy] [downstream] initialize Upstream Connection Pool error, request can't be proxyed, error = %v", err)
		s.requestInfo.SetResponseFlag(types.NoHealthyUpstream)
		s.sendHijackReply(types.NoHealthUpstreamCode, s.downstreamReqHeaders)
		return
	}

	parseProxyTimeout(&s.timeout, s.route, s.downstreamReqHeaders)
	if log.Proxy.GetLogLevel() >= log.DEBUG {
		log.Proxy.Debugf(s.context, "[proxy] [downstream] timeout info: %+v", s.timeout)
	}

	prot := s.getUpstreamProtocol()

	s.retryState = newRetryState(s.route.RouteRule().Policy().RetryPolicy(), s.downstreamReqHeaders, s.cluster, prot)

	//Build Request
	proxyBuffers := proxyBuffersByContext(s.context)
	s.upstreamRequest = &proxyBuffers.request
	s.upstreamRequest.downStream = s
	s.upstreamRequest.proxy = s.proxy
	s.upstreamRequest.protocol = prot
	s.upstreamRequest.connPool = pool
	s.route.RouteRule().FinalizeRequestHeaders(s.downstreamReqHeaders, s.requestInfo)

	//Call upstream's append header method to build upstream's request
	s.upstreamRequest.appendHeaders(endStream)

	if endStream {
		s.onUpstreamRequestSent()
	}
}

func (s *downStream) receiveData(endStream bool) {
	// if active stream finished before receive data, just ignore further data
	if s.processDone() {
		return
	}
	data := s.downstreamReqDataBuf
	if log.Proxy.GetLogLevel() >= log.DEBUG {
		log.Proxy.Debugf(s.context, "[proxy] [downstream] receive data = %v", data)
	}

	s.requestInfo.SetBytesReceived(s.requestInfo.BytesReceived() + uint64(data.Len()))
	s.downstreamRecvDone = endStream

	if endStream {
		s.onUpstreamRequestSent()
	}

	s.upstreamRequest.appendData(endStream)

	// if upstream process done in the middle of receiving data, just end stream
	if s.upstreamProcessDone {
		s.cleanStream()
	}
}

func (s *downStream) receiveTrailers() {
	// if active stream finished the lifecycle, just ignore further data
	if s.processDone() {
		return
	}

	s.downstreamRecvDone = true

	s.onUpstreamRequestSent()
	s.upstreamRequest.appendTrailers()

	// if upstream process done in the middle of receiving trailers, just end stream
	if s.upstreamProcessDone {
		s.cleanStream()
	}
}

func (s *downStream) OnDecodeError(context context.Context, err error, headers types.HeaderMap) {
	// if active stream finished the lifecycle, just ignore further data
	if s.upstreamProcessDone {
		return
	}

	// todo: enrich headers' information to do some hijack
	// Check headers' info to do hijack
	switch err.Error() {
	case types.CodecException:
		s.sendHijackReply(types.CodecExceptionCode, headers)
	case types.DeserializeException:
		s.sendHijackReply(types.DeserialExceptionCode, headers)
	default:
		s.sendHijackReply(types.UnknownCode, headers)
	}
}

func (s *downStream) onUpstreamRequestSent() {
	s.upstreamRequestSent = true
	s.requestInfo.SetRequestReceivedDuration(time.Now())

	if s.upstreamRequest != nil && !s.oneway {
		// setup per req timeout timer
		s.setupPerReqTimeout()

		// setup global timeout timer
		if s.timeout.GlobalTimeout > 0 {
			log.Proxy.Debugf(s.context, "[proxy] [downstream] start a request timeout timer")
			if s.responseTimer != nil {
				s.responseTimer.Stop()
			}

			ID := s.ID
			s.responseTimer = utils.NewTimer(s.timeout.GlobalTimeout,
				func() {
					if atomic.LoadUint32(&s.downstreamCleaned) == 1 {
						return
					}
					if ID != s.ID {
						return
					}
					s.onResponseTimeout()
				})
		}
	}
}

// Note: global-timer MUST be stopped before active stream got recycled, otherwise resetting stream's properties will cause panic here
func (s *downStream) onResponseTimeout() {
	defer func() {
		if r := recover(); r != nil {
			log.Proxy.Errorf(s.context, "[proxy] [downstream] onResponseTimeout() panic %v", r)
		}
	}()
	s.responseTimer = nil
	s.cluster.Stats().UpstreamRequestTimeout.Inc(1)

	if s.upstreamRequest != nil {
		if s.upstreamRequest.host != nil {
			s.upstreamRequest.host.HostStats().UpstreamRequestTimeout.Inc(1)
		}

		atomic.StoreUint32(&s.reuseBuffer, 0)
		s.upstreamRequest.resetStream()
		s.upstreamRequest.OnResetStream(types.UpstreamGlobalTimeout)
	}
}

func (s *downStream) setupPerReqTimeout() {
	timeout := s.timeout

	if timeout.TryTimeout > 0 {
		if s.perRetryTimer != nil {
			s.perRetryTimer.Stop()
		}

		ID := s.ID
		s.perRetryTimer = utils.NewTimer(timeout.TryTimeout,
			func() {
				if atomic.LoadUint32(&s.downstreamCleaned) == 1 {
					return
				}
				if ID != s.ID {
					return
				}
				s.onPerReqTimeout()
			})
	}
}

// Note: per-try-timer MUST be stopped before active stream got recycled, otherwise resetting stream's properties will cause panic here
func (s *downStream) onPerReqTimeout() {
	defer func() {
		if r := recover(); r != nil {
			log.Proxy.Errorf(s.context, "[proxy] [downstream] onPerReqTimeout() panic %v", r)
		}
	}()

	if !s.downstreamResponseStarted {
		// handle timeout on response not

		s.perRetryTimer = nil
		s.cluster.Stats().UpstreamRequestTimeout.Inc(1)

		if s.upstreamRequest.host != nil {
			s.upstreamRequest.host.HostStats().UpstreamRequestTimeout.Inc(1)
		}

		atomic.StoreUint32(&s.reuseBuffer, 0)
		s.upstreamRequest.resetStream()
		s.requestInfo.SetResponseFlag(types.UpstreamRequestTimeout)
		s.upstreamRequest.OnResetStream(types.UpstreamPerTryTimeout)
	} else {
		log.Proxy.Debugf(s.context, "[proxy] [downstream] skip request timeout on getting upstream response")
	}
}

func (s *downStream) initializeUpstreamConnectionPool(lbCtx types.LoadBalancerContext) (types.ConnectionPool, error) {
	var connPool types.ConnectionPool

	currentProtocol := s.getUpstreamProtocol()

	connPool = s.proxy.clusterManager.ConnPoolForCluster(lbCtx, s.snapshot, currentProtocol)

	if connPool == nil {
		return nil, fmt.Errorf("[proxy] [downstream] no healthy upstream in cluster %s", s.cluster.Name())
	}

	// TODO: update upstream stats

	return connPool, nil
}

// ~~~ active stream sender wrapper

func (s *downStream) appendHeaders(endStream bool) {
	s.upstreamProcessDone = endStream
	headers := s.convertHeader(s.downstreamRespHeaders)
	//Currently, just log the error
	if err := s.responseSender.AppendHeaders(s.context, headers, endStream); err != nil {
		log.Proxy.Errorf(s.context, "[proxy] [downstream] append headers error, %s", err)
	}

	if endStream {
		s.endStream()
	}
}

func (s *downStream) convertHeader(headers types.HeaderMap) types.HeaderMap {
	if s.noConvert {
		return headers
	}

	dp, up := s.convertProtocol()

	// need protocol convert
	if dp != up {
		if convHeader, err := protocol.ConvertHeader(s.context, up, dp, headers); err == nil {
			return convHeader
		} else {
			log.Proxy.Warnf(s.context, "[proxy] [downstream] convert header from %s to %s failed, %s", up, dp, err.Error())
		}
	}
	return headers
}

func (s *downStream) appendData(endStream bool) {
	s.upstreamProcessDone = endStream

	data := s.convertData(s.downstreamRespDataBuf)
	s.requestInfo.SetBytesSent(s.requestInfo.BytesSent() + uint64(data.Len()))
	s.responseSender.AppendData(s.context, data, endStream)

	if endStream {
		s.endStream()
	}
}

func (s *downStream) convertData(data types.IoBuffer) types.IoBuffer {
	if s.noConvert {
		return data
	}

	dp, up := s.convertProtocol()

	// need protocol convert
	if dp != up {
		if convData, err := protocol.ConvertData(s.context, up, dp, data); err == nil {
			return convData
		} else {
			log.Proxy.Warnf(s.context, "[proxy] [downstream] convert data from %s to %s failed, %s", up, dp, err.Error())
		}
	}
	return data
}

func (s *downStream) appendTrailers() {
	s.upstreamProcessDone = true
	trailers := s.convertTrailer(s.downstreamRespTrailers)
	s.responseSender.AppendTrailers(s.context, trailers)
	s.endStream()
}

func (s *downStream) convertTrailer(trailers types.HeaderMap) types.HeaderMap {
	if s.noConvert {
		return trailers
	}

	dp, up := s.convertProtocol()

	// need protocol convert
	if dp != up {
		if convTrailer, err := protocol.ConvertTrailer(s.context, up, dp, trailers); err == nil {
			return convTrailer
		} else {
			log.Proxy.Warnf(s.context, "[proxy] [downstream] convert header from %s to %s failed, %s", up, dp, err.Error())
		}
	}
	return trailers
}

// ~~~ upstream event handler
func (s *downStream) onUpstreamReset(reason types.StreamResetReason) {
	// todo: update stats
	log.Proxy.Errorf(s.context, "[proxy] [downstream] onUpstreamReset, reason: %v", reason)

	// see if we need a retry
	if reason != types.UpstreamGlobalTimeout &&
		!s.downstreamResponseStarted && s.retryState != nil {
		retryCheck := s.retryState.retry(nil, reason)

		if retryCheck == types.ShouldRetry && s.setupRetry(true) {
			if s.upstreamRequest != nil && s.upstreamRequest.host != nil {
				s.upstreamRequest.host.HostStats().UpstreamResponseFailed.Inc(1)
				s.upstreamRequest.host.ClusterInfo().Stats().UpstreamResponseFailed.Inc(1)
			}

			// setup retry timer and return
			// clear reset flag
			log.Proxy.Errorf(s.context, "[proxy] [downstream] onUpstreamReset, doRetry, reason %v", reason)
			atomic.CompareAndSwapUint32(&s.upstreamReset, 1, 0)
			return
		} else if retryCheck == types.RetryOverflow {
			s.requestInfo.SetResponseFlag(types.UpstreamOverflow)
		}
	}

	// clean up all timers
	s.cleanUp()

	// If we have not yet sent anything downstream, send a response with an appropriate status code.
	// Otherwise just reset the ongoing response.
	if s.downstreamResponseStarted {
		s.resetStream()
	} else {
		// send err response if response not started
		var code int

		if reason == types.UpstreamGlobalTimeout || reason == types.UpstreamPerTryTimeout {
			s.requestInfo.SetResponseFlag(types.UpstreamRequestTimeout)
			code = types.TimeoutExceptionCode
		} else {
			reasonFlag := s.proxy.streamResetReasonToResponseFlag(reason)
			s.requestInfo.SetResponseFlag(reasonFlag)
			code = types.NoHealthUpstreamCode
		}

		if s.upstreamRequest != nil && s.upstreamRequest.host != nil {
			s.upstreamRequest.host.HostStats().UpstreamResponseFailed.Inc(1)
			s.upstreamRequest.host.ClusterInfo().Stats().UpstreamResponseFailed.Inc(1)
		}
		// clear reset flag
		log.Proxy.Errorf(s.context, "[proxy] [downstream] onUpstreamReset, send hijack, reason %v", reason)
		atomic.CompareAndSwapUint32(&s.upstreamReset, 1, 0)
		s.sendHijackReply(code, s.downstreamReqHeaders)
	}
}

func (s *downStream) onUpstreamHeaders(endStream bool) {
	headers := s.downstreamRespHeaders

	// check retry
	if s.retryState != nil {
		retryCheck := s.retryState.retry(headers, "")

		if retryCheck == types.ShouldRetry && s.setupRetry(endStream) {
			if s.upstreamRequest != nil && s.upstreamRequest.host != nil {
				s.upstreamRequest.host.HostStats().UpstreamResponseFailed.Inc(1)
				s.upstreamRequest.host.ClusterInfo().Stats().UpstreamResponseFailed.Inc(1)
			}

			return
		} else if retryCheck == types.RetryOverflow {
			s.requestInfo.SetResponseFlag(types.UpstreamOverflow)
		}

		s.retryState.reset()
	}

	s.handleUpstreamStatusCode()

	s.downstreamResponseStarted = true

	// directResponse for no route should be nil
	if s.route != nil {
		s.route.RouteRule().FinalizeResponseHeaders(headers, s.requestInfo)
	}

	if endStream {
		s.onUpstreamResponseRecvFinished()
	}

	// todo: insert proxy headers
	s.appendHeaders(endStream)
}

func (s *downStream) handleUpstreamStatusCode() {
	// todo: support config?
	if s.upstreamRequest != nil && s.upstreamRequest.host != nil {
		if s.requestInfo.ResponseCode() >= http.InternalServerError {
			s.upstreamRequest.host.HostStats().UpstreamResponseFailed.Inc(1)
			s.upstreamRequest.host.ClusterInfo().Stats().UpstreamResponseFailed.Inc(1)
		} else {
			s.upstreamRequest.host.HostStats().UpstreamResponseSuccess.Inc(1)
			s.upstreamRequest.host.ClusterInfo().Stats().UpstreamResponseSuccess.Inc(1)
		}
	}
}

func (s *downStream) onUpstreamData(endStream bool) {
	if endStream {
		s.onUpstreamResponseRecvFinished()
	}

	s.appendData(endStream)
}

func (s *downStream) finishTracing() {
	if trace.IsTracingEnabled() {
		if s.context == nil {
			return
		}
		span := trace.SpanFromContext(s.context)

		if span != nil {
			span.SetTag(trace.REQUEST_SIZE, strconv.FormatInt(int64(s.requestInfo.BytesSent()), 10))
			span.SetTag(trace.RESPONSE_SIZE, strconv.FormatInt(int64(s.requestInfo.BytesReceived()), 10))
			if s.requestInfo.UpstreamHost() != nil {
				span.SetTag(trace.UPSTREAM_HOST_ADDRESS, s.requestInfo.UpstreamHost().AddressString())
			}
			if s.requestInfo.DownstreamLocalAddress() != nil {
				span.SetTag(trace.DOWNSTEAM_HOST_ADDRESS, s.requestInfo.DownstreamRemoteAddress().String())
			}
			span.SetTag(trace.RESULT_STATUS, strconv.Itoa(s.requestInfo.ResponseCode()))
			span.SetRequestInfo(s.requestInfo)
			span.FinishSpan()

			if mosnctx.Get(s.context, types.ContextKeyListenerType) == v2.INGRESS {
				trace.DeleteSpanIdGenerator(mosnctx.Get(s.context, types.ContextKeyTraceSpanKey).(*trace.SpanKey))
			}
		} else {
			log.Proxy.Warnf(s.context, "[proxy] [downstream] trace span is null")
		}
	}
}

func (s *downStream) onUpstreamTrailers() {
	s.onUpstreamResponseRecvFinished()

	s.appendTrailers()
}

func (s *downStream) onUpstreamResponseRecvFinished() {
	if !s.upstreamRequestSent {
		s.upstreamRequest.resetStream()
	}

	// todo: stats
	// todo: logs

	s.cleanUp()
}

func (s *downStream) setupRetry(endStream bool) bool {
	s.upstreamRequest.setupRetry = true

	if !endStream {
		s.upstreamRequest.resetStream()
	}

	// reset per req timer
	if s.perRetryTimer != nil {
		s.perRetryTimer.Stop()
		s.perRetryTimer = nil
	}

	return true
}

// Note: retry-timer MUST be stopped before active stream got recycled, otherwise resetting stream's properties will cause panic here
func (s *downStream) doRetry() {
	// no reuse buffer
	atomic.StoreUint32(&s.reuseBuffer, 0)

	pool, err := s.initializeUpstreamConnectionPool(s)

	if err != nil {
		log.Proxy.Errorf(s.context, "[proxy] [downstream] retry choose conn pool failed, error = %v", err)
		s.sendHijackReply(types.NoHealthUpstreamCode, s.downstreamReqHeaders)
		s.cleanUp()
		return
	}

	s.upstreamRequest = &upstreamRequest{
		downStream: s,
		proxy:      s.proxy,
		connPool:   pool,
	}

	// if Data or Trailer exists, endStream should be false, else should be true
	s.upstreamRequest.appendHeaders(s.downstreamReqDataBuf == nil && s.downstreamReqTrailers == nil)

	if s.downstreamReqDataBuf != nil {
		s.upstreamRequest.appendData(s.downstreamReqTrailers == nil)
	}

	if s.downstreamReqTrailers != nil {
		s.upstreamRequest.appendTrailers()
	}

	// setup per try timeout timer
	s.setupPerReqTimeout()

	s.upstreamRequestSent = true
	s.downstreamRecvDone = true
}

// Downstream got reset in proxy context on scenario below:
// 1. downstream filter reset downstream
// 2. corresponding upstream got reset
func (s *downStream) resetStream() {
	if s.responseSender != nil && !s.upstreamProcessDone {
		// if downstream req received not done, or local proxy process not done by handle upstream response,
		// just mark it as done and reset stream as a failed case
		s.upstreamProcessDone = true

		// reset downstream will trigger a clean up, see OnResetStream
		s.responseSender.GetStream().ResetStream(types.StreamLocalReset)
	}
}

func (s *downStream) sendHijackReply(code int, headers types.HeaderMap) {
	log.Proxy.Errorf(s.context, "[proxy] [downstream] set hijack reply, proxyId = %d, code = %d", s.ID, code)
	if headers == nil {
		log.Proxy.Warnf(s.context, "[proxy] [downstream] hijack with no headers, proxyId = %d", s.ID)
		raw := make(map[string]string, 5)
		headers = protocol.CommonHeader(raw)
	}
	s.requestInfo.SetResponseCode(code)

	headers.Set(types.HeaderStatus, strconv.Itoa(code))
	atomic.StoreUint32(&s.reuseBuffer, 0)
	s.downstreamRespHeaders = headers
	s.downstreamRespDataBuf = nil
	s.downstreamRespTrailers = nil
	s.directResponse = true
}

// TODO: rpc status code may be not matched
// TODO: rpc content(body) is not matched the headers, rpc should not hijack with body, use sendHijackReply instead
func (s *downStream) sendHijackReplyWithBody(code int, headers types.HeaderMap, body string) {
	log.Proxy.Errorf(s.context, "[proxy] [downstream] set hijack reply with body, proxyId = %d, code = %d", s.ID, code)
	if headers == nil {
		log.Proxy.Warnf(s.context, "[proxy] [downstream] hijack with no headers, proxyId = %d", s.ID)
		raw := make(map[string]string, 5)
		headers = protocol.CommonHeader(raw)
	}
	s.requestInfo.SetResponseCode(code)
	headers.Set(types.HeaderStatus, strconv.Itoa(code))
	atomic.StoreUint32(&s.reuseBuffer, 0)
	s.downstreamRespHeaders = headers
	s.downstreamRespDataBuf = buffer.NewIoBufferString(body)
	s.downstreamRespTrailers = nil
	s.directResponse = true
}

func (s *downStream) cleanUp() {
	// reset retry state
	// if  a downstream filter ends downstream before send to upstream, retryState will be nil
	if s.retryState != nil {
		s.retryState.reset()
	}

	// reset pertry timer
	if s.perRetryTimer != nil {
		s.perRetryTimer.Stop()
		s.perRetryTimer = nil
	}

	// reset response timer
	if s.responseTimer != nil {
		s.responseTimer.Stop()
		s.responseTimer = nil
	}

}

func (s *downStream) setBufferLimit(bufferLimit uint32) {
	s.bufferLimit = bufferLimit

	// todo
}

func (s *downStream) AddStreamReceiverFilter(filter types.StreamReceiverFilter, p types.Phase) {
	sf := newActiveStreamReceiverFilter(s, filter, p)
	s.receiverFilters = append(s.receiverFilters, sf)
}

func (s *downStream) AddStreamSenderFilter(filter types.StreamSenderFilter) {
	sf := newActiveStreamSenderFilter(s, filter)
	s.senderFilters = append(s.senderFilters, sf)
}

func (s *downStream) AddStreamAccessLog(accessLog types.AccessLog) {
	if s.proxy != nil {
		if s.streamAccessLogs == nil {
			s.streamAccessLogs = make([]types.AccessLog, 0)
		}
		s.streamAccessLogs = append(s.streamAccessLogs, accessLog)
	}
}

// types.LoadBalancerContext
// no use currently
func (s *downStream) ComputeHashKey() types.HashedValue {
	//return [16]byte{}
	return ""
}

func (s *downStream) MetadataMatchCriteria() types.MetadataMatchCriteria {
	if nil != s.requestInfo.RouteEntry() {
		return s.requestInfo.RouteEntry().MetadataMatchCriteria(s.cluster.Name())
	}

	return nil
}

func (s *downStream) DownstreamConnection() net.Conn {
	return s.proxy.readCallbacks.Connection().RawConn()
}

func (s *downStream) DownstreamHeaders() types.HeaderMap {
	return s.downstreamReqHeaders
}

func (s *downStream) DownstreamContext() context.Context {
	return s.context
}

func (s *downStream) giveStream() {
	if s.snapshot != nil {
		s.proxy.clusterManager.PutClusterSnapshot(s.snapshot)
	}
	if atomic.LoadUint32(&s.reuseBuffer) != 1 {
		return
	}
	if atomic.LoadUint32(&s.upstreamReset) == 1 || atomic.LoadUint32(&s.downstreamReset) == 1 {
		return
	}

	if log.Proxy.GetLogLevel() >= log.DEBUG {
		log.Proxy.Debugf(s.context, "[proxy] [downstream] giveStream %p %+v", s, s)
	}

	// reset downstreamReqBuf
	if s.downstreamReqDataBuf != nil {
		if e := buffer.PutIoBuffer(s.downstreamReqDataBuf); e != nil {
			log.Proxy.Errorf(s.context, "[proxy] [downstream] PutIoBuffer error: %v", e)
		}
	}

	// Give buffers to bufferPool
	if ctx := buffer.PoolContext(s.context); ctx != nil {
		ctx.Give()
	}
}

// check if proxy process done
func (s *downStream) processDone() bool {
	return s.upstreamProcessDone || atomic.LoadUint32(&s.downstreamReset) == 1 || atomic.LoadUint32(&s.upstreamReset) == 1
}

func (s *downStream) sendNotify() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *downStream) cleanNotify() {
	select {
	case <-s.notify:
	default:
	}
}

func (s *downStream) waitNotify(id uint32) (phase types.Phase, err error) {
	if s.ID != id {
		return types.End, types.ErrExit
	}

	if log.Proxy.GetLogLevel() >= log.DEBUG {
		log.Proxy.Debugf(s.context, "[proxy] [downstream] waitNotify begin %p, proxyId = %d", s, s.ID)
	}
	select {
	case <-s.notify:
	}
	return s.processError(id)
}

func (s *downStream) processError(id uint32) (phase types.Phase, err error) {
	if s.ID != id {
		return types.End, types.ErrExit
	}

	phase = types.End

	if atomic.LoadUint32(&s.downstreamCleaned) == 1 {
		err = types.ErrExit
		return
	}

	if atomic.LoadUint32(&s.upstreamReset) == 1 {
		log.Proxy.Errorf(s.context, "[proxy] [downstream] processError=upstreamReset, proxyId: %d", s.ID)
		s.onUpstreamReset(s.resetReason)
		err = types.ErrExit
	}

	if atomic.LoadUint32(&s.downstreamReset) == 1 {
		log.Proxy.Errorf(s.context, "[proxy] [downstream] processError=downstreamReset proxyId: %d", s.ID)
		s.ResetStream(s.resetReason)
		err = types.ErrExit
		return
	}

	if s.directResponse {
		s.directResponse = false
		if s.oneway {
			phase = types.Oneway
		} else {
			phase = types.UpFilter
		}
		err = types.ErrExit
		return
	}

	if s.upstreamProcessDone {
		err = types.ErrExit
	}

	if s.upstreamRequest != nil && s.upstreamRequest.setupRetry {
		phase = types.Retry
		err = types.ErrExit
		return
	}

	if s.receiverFiltersAgain {
		s.receiverFiltersAgain = false
		phase = types.MatchRoute
		err = types.ErrExit
		return
	}

	return
}
