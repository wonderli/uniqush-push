/*
 * Copyright 2011 Nan Deng
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package uniqush


import taskq "github.com/monnand/gotaskqueue"
import "os"

type retryPushTask struct {
    taskq.TaskTime
    req *Request
    backendch chan<- *Request
}

func (t *retryPushTask) Run(time int64) {
    t.backendch <- t.req
}

type PushProcessor struct {
    loggerEventWriter
    databaseSetter
    max_nr_gorountines int
    max_nr_retry int
    q *taskq.TaskQueue
    qch chan taskq.Task
    backendch chan<- *Request
    psm *PushServiceManager
}

const (
    init_backoff_time = 3
)

func (p *PushProcessor) retryRequest(req *Request, retryAfter int, subscriber string, psp *PushServiceProvider, dp *DeliveryPoint) {
    if req.nrRetries >= p.max_nr_retry {
        return
    }
    newreq := new(Request)
    newreq.nrRetries = req.nrRetries + 1
    newreq.PreviousTry = req
    newreq.ID = req.ID
    newreq.Action = ACTION_PUSH
    newreq.PushServiceProvider = psp
    newreq.DeliveryPoint = dp
    newreq.RequestSenderAddr = req.RequestSenderAddr
    newreq.Notification = req.Notification

    newreq.Service = req.Service
    newreq.Subscribers = make([]string, 1)
    newreq.Subscribers[0] = subscriber
    newreq.PunchTimestamp()

    if req.nrRetries == 0 || req.backoffTime == 0 {
        newreq.backoffTime = init_backoff_time
    } else {
        newreq.backoffTime = req.backoffTime << 1
    }

    task := new(retryPushTask)
    task.backendch = p.backendch
    task.req = newreq

    if retryAfter <= 0 {
        task.After(newreq.backoffTime)
    } else {
        task.SetExecTime(int64(retryAfter)* 1E9)
    }

    p.qch <- task
}

func NewPushProcessor(logger *Logger,
                      writer *EventWriter,
                      dbfront DatabaseFrontDeskIf,
                      backendch chan<- *Request,
                      psm *PushServiceManager) RequestProcessor{
    ret := new(PushProcessor)
    ret.SetLogger(logger)
    ret.SetEventWriter(writer)
    ret.SetDatabase(dbfront)
    ret.max_nr_gorountines = 1024
    ret.max_nr_retry = 3
    ret.qch = make(chan taskq.Task)
    ret.q = taskq.NewTaskQueue(ret.qch)
    ret.backendch = backendch

    go ret.q.Run()

    return ret
}

func (p *PushProcessor) unsubscribe(req *Request, subscriber string, dp *DeliveryPoint) {
    a := new(Request)
    a.PunchTimestamp()
    a.ID = req.ID
    a.Action = ACTION_UNSUBSCRIBE
    a.RequestSenderAddr = req.RequestSenderAddr
    a.Service = req.Service
    a.Subscribers = make([]string, 1)
    a.Subscribers[0] = subscriber
    a.DeliveryPoint = dp
    p.backendch <- a
}

func (p *PushProcessor) pushToDeliveryPoint(req *Request,
                                            service string,
                                            subscriber string,
                                            psp *PushServiceProvider,
                                            dp *DeliveryPoint) {
        id, err := p.psm.Push(psp, dp, req.Notification)
        if err != nil {
            switch (err.(type)) {
            case *RefreshDataError:
                re := err.(*RefreshDataError)
                err = p.refreshData(req, psp.PushServiceName(), re)
                if err == nil {
                    return
                }
            }
            switch (err.(type)) {
            case *RetryError:
                re := err.(*RetryError)
                go p.pushRetry(req, subscriber, psp, dp, re)
            case *UnregisteredError:
                go p.unsubscribe(req, subscriber, dp)
            }
            go p.pushFail(req, subscriber, psp, dp, err)
        }
        go p.pushSucc(req, subscriber, psp, dp, id)

}

func (p *PushProcessor) push(req *Request, service string, subscriber string) {
    pspdppairs, err := p.dbfront.GetPushServiceProviderDeliveryPointPairs(service, subscriber)
    if err != nil {
        p.logger.Errorf("[PushFail] Service=%s Subscriber=%s DatabaseError %v", service, subscriber, err)
        p.writer.PushFail(req, subscriber, nil, nil, err)
    }
    if len(pspdppairs) <= 0 {
        p.logger.Warnf("[PushFail] Service=%s Subscriber=%s NoSubscriber", service, subscriber)
        return
    }

    for _, pdpair := range pspdppairs {
        psp := pdpair.PushServiceProvider
        dp := pdpair.DeliveryPoint
        p.pushToDeliveryPoint(req, service, subscriber, psp, dp)
    }
}

func (p *PushProcessor) refreshData(req *Request,
                                    stype string,
                                    re *RefreshDataError) os.Error {
    if re.PushServiceProvider != nil {
        p.dbfront.ModifyPushServiceProvider(re.PushServiceProvider)
        p.logger.Infof("[%s][UpdatePushServiceProvider] Service=%s PushServiceProvider=%s",
                       stype, req.Service, re.PushServiceProvider.Name())
    }
    if re.DeliveryPoint != nil {
        p.dbfront.ModifyDeliveryPoint(re.DeliveryPoint)
        p.logger.Infof("[%s][UpdateDeliveryPoint] DeliveryPoint=%s",
                       stype, re.DeliveryPoint.Name())
    }
    return re.OtherError
}

func (p *PushProcessor) pushFail(req *Request,
                            subscriber string,
                            psp *PushServiceProvider,
                            dp *DeliveryPoint,
                            err os.Error) {
    p.logger.Errorf("[%s][PushFail] RequestId=%s Service=%s Subscriber=%s PushServiceProvider=%s DeliveryPoint=%s \"%v\"",
                   psp.PushServiceName(), req.ID, req.Service, subscriber,
                   psp.Name(), dp.Name(), err)
    p.writer.PushFail(req, subscriber, psp, dp, err)
}

func (p *PushProcessor) pushRetry(req *Request,
                            subscriber string,
                            psp *PushServiceProvider,
                            dp *DeliveryPoint,
                            err *RetryError) {
    p.retryRequest(req, err.RetryAfter, subscriber, psp, dp)
    p.logger.Warnf("[%s][PushRetry] RequestId=%s Service=%s Subscriber=%s PushServiceProvider=%s DeliveryPoint=%s \"%v\"",
                   psp.PushServiceName(), req.ID, req.Service, subscriber,
                   psp.Name(), dp.Name(), err)
}

func (p *PushProcessor) pushSucc(req *Request,
                            subscriber string,
                            psp *PushServiceProvider,
                            dp *DeliveryPoint,
                            id string) {
    p.logger.Warnf("[%s][PushSuccess] RequestId=%s Service=%s Subscriber=%s PushServiceProvider=%s DeliveryPoint=%s MsgId=%s",
                   psp.PushServiceName(), req.ID, req.Service, subscriber,
                   psp.Name(), dp.Name(), id)
}

func (p *PushProcessor) pushBulk(req *Request, service string, subscribers []string, finish chan bool) {
    for _, sub := range subscribers {
        p.push(req, service, sub)
    }
    if finish != nil {
        finish <- true
    }
}

func (p *PushProcessor) Process(req *Request) {
    nr_subs_per_goroutine := len(req.Subscribers) / p.max_nr_gorountines
    nr_subs_last_goroutine := len(req.Subscribers) % p.max_nr_gorountines
    nr_goroutines := 0
    finish := make(chan bool)
    pos := 0

    if len(req.Subscribers) == 1 && req.PushServiceProvider != nil && req.DeliveryPoint != nil {
        p.pushToDeliveryPoint(req, req.Service, req.Subscribers[0], req.PushServiceProvider, req.DeliveryPoint)
        return
    }

    for pos = 0; pos < len(req.Subscribers) - nr_subs_last_goroutine; pos += nr_subs_per_goroutine {
        go p.pushBulk(req, req.Service, req.Subscribers[pos:pos + nr_subs_per_goroutine], finish)
        nr_goroutines += 1
    }
    if pos < len(req.Subscribers) {
        p.pushBulk(req, req.Service, req.Subscribers[pos:], nil)
    }

    for i := 0; i < nr_goroutines; i++ {
        <-finish
    }
}

