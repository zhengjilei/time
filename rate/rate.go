// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package rate provides a rate limiter.
package rate

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

// Limit defines the maximum frequency of some events.
// Limit is represented as number of events per second.
// A zero Limit allows no events.
type Limit float64

// Inf is the infinite rate limit; it allows all events (even if burst is zero).
const Inf = Limit(math.MaxFloat64)

// Every converts a minimum time interval between events to a Limit.
// interval: 每个请求耗时多长？  1/interval(转换成秒) ->  每 s 多少个请求
func Every(interval time.Duration) Limit {
	if interval <= 0 {
		return Inf
	}
	return 1 / Limit(interval.Seconds())
}

// A Limiter controls how frequently events are allowed to happen.
// It implements a "token bucket" of size b, initially full and refilled
// at rate r tokens per second.
// Informally, in any large enough time interval, the Limiter limits the
// rate to r tokens per second, with a maximum burst size of b events.
// As a special case, if r == Inf (the infinite rate), b is ignored.
// See https://en.wikipedia.org/wiki/Token_bucket for more about token buckets.
//
// The zero value is a valid Limiter, but it will reject all events.
// Use NewLimiter to create non-zero Limiters.
//
// Limiter has three main methods, Allow, Reserve, and Wait.
// Most callers should use Wait.
//
// Each of the three methods consumes a single token.
// They differ in their behavior when no token is available.
// If no token is available, Allow returns false.
// If no token is available, Reserve returns a reservation for a future token
// and the amount of time the caller must wait before using it.
// If no token is available, Wait blocks until one can be obtained
// or its associated context.Context is canceled.
//
// The methods AllowN, ReserveN, and WaitN consume n tokens.
type Limiter struct {
	mu     sync.Mutex
	limit  Limit   // 限制每秒多少个请求
	burst  int     // 桶能容纳的token总数，允许突发流量 burst 个请求
	tokens float64 // 桶中现有的 token 总数
	// last is the last time the limiter's tokens field was updated
	last time.Time // 上一次 tokens 字段更新的时间戳
	// lastEvent is the latest time of a rate-limited event (past or future)
	lastEvent time.Time // 上一次 rate-limit event 发生的时间戳（之前的，也可能是将来的 - 因为 reservation ）
}

// Limit returns the maximum overall event rate.
func (lim *Limiter) Limit() Limit {
	lim.mu.Lock()
	defer lim.mu.Unlock()
	return lim.limit
}

// Burst returns the maximum burst size. Burst is the maximum number of tokens
// that can be consumed in a single call to Allow, Reserve, or Wait, so higher
// Burst values allow more events to happen at once.
// A zero Burst allows no events, unless limit == Inf.
func (lim *Limiter) Burst() int {
	lim.mu.Lock()
	defer lim.mu.Unlock()
	return lim.burst
}

// NewLimiter returns a new Limiter that allows events up to rate r and permits
// bursts of at most b tokens.
func NewLimiter(r Limit, b int) *Limiter {
	return &Limiter{
		limit: r,
		burst: b,
	}
}

// Allow is shorthand for AllowN(time.Now(), 1).
func (lim *Limiter) Allow() bool {
	return lim.AllowN(time.Now(), 1)
}

// AllowN reports whether n events may happen at time now.
// Use this method if you intend to drop / skip events that exceed the rate limit.
// Otherwise use Reserve or Wait.
// 当超出 当前可使用的 token 个数，立即返回 false，不会等待
func (lim *Limiter) AllowN(now time.Time, n int) bool {
	return lim.reserveN(now, n, 0).ok
}

// A Reservation holds information about events that are permitted by a Limiter to happen after a delay.
// A Reservation may be canceled, which may enable the Limiter to permit additional events.
type Reservation struct {
	ok        bool
	lim       *Limiter
	tokens    int
	timeToAct time.Time // reserve 成功后，真正执行的时间
	// This is the Limit at reservation time, it can change later.
	limit Limit
}

// OK returns whether the limiter can provide the requested number of tokens
// within the maximum wait time.  If OK is false, Delay returns InfDuration, and
// Cancel does nothing.
func (r *Reservation) OK() bool {
	return r.ok
}

// 计算从当前时刻还需要多久才能执行 reservation
// Delay is shorthand for DelayFrom(time.Now()).
func (r *Reservation) Delay() time.Duration {
	return r.DelayFrom(time.Now())
}

// InfDuration is the duration returned by Delay when a Reservation is not OK.
const InfDuration = time.Duration(1<<63 - 1)

// DelayFrom returns the duration for which the reservation holder must wait
// before taking the reserved action.  Zero duration means act immediately.
// InfDuration means the limiter cannot grant the tokens requested in this
// Reservation within the maximum wait time.
func (r *Reservation) DelayFrom(now time.Time) time.Duration {
	if !r.ok {
		return InfDuration
	}
	// 预期拿到 reserved token 的时刻 距离现在的 delay
	delay := r.timeToAct.Sub(now)
	if delay < 0 {
		return 0
	}
	return delay
}

// Cancel is shorthand for CancelAt(time.Now()).
func (r *Reservation) Cancel() {
	r.CancelAt(time.Now())
	return
}

// 取消 reservation, （恢复token的值）
// CancelAt indicates that the reservation holder will not perform the reserved action
// and reverses the effects of this Reservation on the rate limit as much as possible,
// considering that other reservations may have already been made.
func (r *Reservation) CancelAt(now time.Time) {
	if !r.ok {
		return
	}

	r.lim.mu.Lock()
	defer r.lim.mu.Unlock()

	// 无限制 or 预订的 token 数量为 0 or ?
	if r.lim.limit == Inf || r.tokens == 0 || r.timeToAct.Before(now) {
		return
	}

	// restore token
	// calculate tokens to restore
	// The duration between lim.lastEvent and r.timeToAct tells us how many tokens were reserved
	// after r was obtained. These tokens should not be restored.
	restoreTokens := float64(r.tokens) - r.limit.tokensFromDuration(r.lim.lastEvent.Sub(r.timeToAct))
	// 对于 reserve 成功的，r.lim.lastEvent = r.timeToAct, 所以 restoreToken = r.tokens
	if restoreTokens <= 0 {
		return
	}
	// advance time to now
	// 从现在到上一次update token 的时间，bucket现在本应该有的token总数
	now, _, tokens := r.lim.advance(now)
	// calculate new number of tokens
	tokens += restoreTokens
	if burst := float64(r.lim.burst); tokens > burst {
		tokens = burst
	}
	// update state
	r.lim.last = now
	r.lim.tokens = tokens
	if r.timeToAct == r.lim.lastEvent { // reserve 成功
		prevEvent := r.timeToAct.Add(r.limit.durationFromTokens(float64(-r.tokens))) // 计算上一次event 的时间
		if !prevEvent.Before(now) {
			r.lim.lastEvent = prevEvent // 设置 event 为上一次 event 的时间
		}
	}

	return
}

// Reserve is shorthand for ReserveN(time.Now(), 1).
func (lim *Limiter) Reserve() *Reservation {
	return lim.ReserveN(time.Now(), 1)
}

// ReserveN returns a Reservation that indicates how long the caller must wait before n events happen.
// The Limiter takes this Reservation into account when allowing future events.
// The returned Reservation’s OK() method returns false if n exceeds the Limiter's burst size.
// Usage example:
//   r := lim.ReserveN(time.Now(), 1)
//   if !r.OK() {
//     // Not allowed to act! Did you remember to set lim.burst to be > 0 ?
//     return
//   }
//   time.Sleep(r.Delay())
//   Act()
// Use this method if you wish to wait and slow down in accordance with the rate limit without dropping events.
// If you need to respect a deadline or cancel the delay, use Wait instead.
// To drop or skip events exceeding rate limit, use Allow instead.
func (lim *Limiter) ReserveN(now time.Time, n int) *Reservation {
	r := lim.reserveN(now, n, InfDuration)
	return &r
}

// Wait is shorthand for WaitN(ctx, 1).
func (lim *Limiter) Wait(ctx context.Context) (err error) {
	return lim.WaitN(ctx, 1)
}

// WaitN blocks until lim permits n events to happen.
// It returns an error if n exceeds the Limiter's burst size, the Context is
// canceled, or the expected wait time exceeds the Context's Deadline.
// The burst limit is ignored if the rate limit is Inf.
func (lim *Limiter) WaitN(ctx context.Context, n int) (err error) {
	lim.mu.Lock()
	burst := lim.burst
	limit := lim.limit
	lim.mu.Unlock()

	if n > burst && limit != Inf { // 一次性要的 token 大于 bucket 总容量，返回 error
		return fmt.Errorf("rate: Wait(n=%d) exceeds limiter's burst %d", n, burst)
	}
	// Check if ctx is already cancelled
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	// Determine wait limit
	now := time.Now()
	waitLimit := InfDuration
	if deadline, ok := ctx.Deadline(); ok { // 有 deadline
		waitLimit = deadline.Sub(now) // 计算距离 deadline 的时间
	}
	// Reserve  返回 Reservation
	r := lim.reserveN(now, n, waitLimit) // 排他锁
	if !r.ok {                           // reservation 失败
		return fmt.Errorf("rate: Wait(n=%d) would exceed context deadline", n)
	}
	// Wait if necessary
	delay := r.DelayFrom(now)
	if delay == 0 { // 没有 delay，返回 nil，说明直接拿到 token 成功，不需要 reservation
		return nil
	}
	t := time.NewTimer(delay) //启动定时器
	defer t.Stop()
	select {
	case <-t.C: // 时间到了，所需要的 token 应该按照 limit 速率都应该加到 bucket 中了（实际没有任何操作）， 说明 reservation 成功
		// We can proceed.
		return nil
	case <-ctx.Done(): // context 取消
		// Context was canceled before we could proceed.  Cancel the
		// reservation, which may permit other events to proceed sooner.
		r.Cancel() // context 先超时，则取消 reservation
		return ctx.Err()
	}
}

// SetLimit is shorthand for SetLimitAt(time.Now(), newLimit).
func (lim *Limiter) SetLimit(newLimit Limit) {
	lim.SetLimitAt(time.Now(), newLimit)
}

// SetLimitAt sets a new Limit for the limiter. The new Limit, and Burst, may be violated
// or underutilized by those which reserved (using Reserve or Wait) but did not yet act
// before SetLimitAt was called.
func (lim *Limiter) SetLimitAt(now time.Time, newLimit Limit) {
	lim.mu.Lock()
	defer lim.mu.Unlock()

	now, _, tokens := lim.advance(now)

	lim.last = now
	lim.tokens = tokens
	lim.limit = newLimit
}

// SetBurst is shorthand for SetBurstAt(time.Now(), newBurst).
func (lim *Limiter) SetBurst(newBurst int) {
	lim.SetBurstAt(time.Now(), newBurst)
}

// SetBurstAt sets a new burst size for the limiter.
func (lim *Limiter) SetBurstAt(now time.Time, newBurst int) {
	lim.mu.Lock()
	defer lim.mu.Unlock()

	now, _, tokens := lim.advance(now)

	lim.last = now
	lim.tokens = tokens
	lim.burst = newBurst
}

// reserveN is a helper method for AllowN, ReserveN, and WaitN.
// maxFutureReserve specifies the maximum reservation wait duration allowed.
//		允许的最大的等待时间
// reserveN returns Reservation, not *Reservation, to avoid allocation in AllowN and WaitN.
func (lim *Limiter) reserveN(now time.Time, n int, maxFutureReserve time.Duration) Reservation {
	lim.mu.Lock()

	if lim.limit == Inf { // 无限制
		lim.mu.Unlock()
		return Reservation{
			ok:        true,
			lim:       lim,
			tokens:    n,
			timeToAct: now,
		}
	}

	// 传入: now 时间戳
	// now: 当前时间戳
	// last: 上一次update token的时间戳
	// tokens: 从上一次 update token 到现在，此时 桶里应该有的 token 总数（算上这段 gap 应该添加的 token 数量 ）
	now, last, tokens := lim.advance(now)

	// Calculate the remaining number of tokens resulting from the request.
	tokens -= float64(n) // 拿去 n 个 token，剩余的 token 数量

	// Calculate the wait duration
	var waitDuration time.Duration
	if tokens < 0 { // 桶内不够 n 个 token, 得到的负数（缺的 token 数量）
		waitDuration = lim.limit.durationFromTokens(-tokens) // 根据缺的 token 数量计算得到 需要等待的时间
	}

	// Decide result
	ok := n <= lim.burst && waitDuration <= maxFutureReserve
	// 如果 1.n <= 桶内最大容量 且 2. 真实需要等待的时间 比 最大需要等待的时间 相等或更短 => 说明 reserve 成功

	// Prepare reservation
	r := Reservation{
		ok:    ok,
		lim:   lim,
		limit: lim.limit,
	}
	if ok {
		r.tokens = n
		r.timeToAct = now.Add(waitDuration) // 预期可以拿到所有需要的 token 的时刻
	}

	// Update state
	if ok {
		lim.last = now
		lim.tokens = tokens
		lim.lastEvent = r.timeToAct // 最近一次 event 的时刻（可能是之后发生）
	} else {
		// reserve 不成功，last 保持不变
		lim.last = last
	}

	lim.mu.Unlock()
	return r
}

// advance: 预付款，返回 当前时间戳，上一次update token的时间戳，以及 从上一次添加到现在，此时 bucket 里应该有的 token 总数（算上这段 gap 应该添加的 token 数量 ）
// advance calculates and returns an updated state for lim resulting from the passage of time.
// lim is not changed.
// advance requires that lim.mu is held.
func (lim *Limiter) advance(now time.Time) (newNow time.Time, newLast time.Time, newTokens float64) {
	last := lim.last
	if now.Before(last) {
		last = now
	}

	// Avoid making delta overflow below when last is very old. 避免因为 last 太小，导致 now - last 的 delta(差值)溢出
	maxElapsed := lim.limit.durationFromTokens(float64(lim.burst) - lim.tokens) // 要填满 bucket，还需要的时间(ns)
	elapsed := now.Sub(last)                                                    // 当前距离上一次token变化的间隔
	if elapsed > maxElapsed {
		elapsed = maxElapsed // 如果距离上一次token 变化的间隔 比填满 bucket 需要的时间(maxElapsed)还长， 直接取 maxElapsed 即可，更长的时间没有意义，桶已经满了
	}

	// Calculate the new number of tokens, due to time that passed.

	// 根据 elapsed 时间，按照添加token的速率，得到需要添加的 token 数量
	delta := lim.limit.tokensFromDuration(elapsed)

	// 为什么像上面这么算需要添加的 token 总数？
	// 如果直接拿 now.Sub(last) * lim.limit  （在过去的时间间隔内 按照速率 limit 添加token，这段时间应该加的 token），由于 last 距离现在可能过大，这样可能会溢出

	tokens := lim.tokens + delta
	if burst := float64(lim.burst); tokens > burst {
		// 如果当前已有的 token 总数 + 要添加的 token 总数 超过 bucket的总容量 ，则 总 token 设置为 burst，保证不超过bucket 的总容量
		tokens = burst
	}

	return now, last, tokens
}

// token / limit (每1s产生的token数量)  -> 产生这么多 token 需要的时间(秒), 返回的单位是 ns
// durationFromTokens is a unit conversion function from the number of tokens to the duration
// of time it takes to accumulate them at a rate of limit tokens per second.
func (limit Limit) durationFromTokens(tokens float64) time.Duration {
	seconds := tokens / float64(limit)
	return time.Nanosecond * time.Duration(1e9*seconds)
}

// limit (每1s产生的 token数量) * time (多少秒) -> 这么长时间能产生的 token 总数
// tokensFromDuration is a unit conversion function from a time duration to the number of tokens
// which could be accumulated during that duration at a rate of limit tokens per second.
func (limit Limit) tokensFromDuration(d time.Duration) float64 {
	// Refer: https://go-review.googlesource.com/c/time/+/200900/6/rate/rate.go#b390
	//	return d.Seconds() * float64(limit)

	// Split the integer and fractional parts ourself to minimize rounding errors.
	// See golang.org/issues/34861.
	sec := float64(d/time.Second) * float64(limit)
	nsec := float64(d%time.Second) * float64(limit)
	return sec + nsec/1e9
}
