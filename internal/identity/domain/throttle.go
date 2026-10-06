package domain

import "time"

// ThrottlePolicy is the per-account login throttling (SR-05, TECHNICAL_SPEC
// §5.12): consecutive failures past DelayAfter impose a progressive delay
// (1 s, 2 s, 4 s, …); from LockAfter the account is locked for LockFor,
// doubling on every further failure, up to MaxLock. While delayed or locked,
// attempts are refused without checking the password and are not counted.
type ThrottlePolicy struct {
	delayAfter int
	lockAfter  int
	lockFor    time.Duration
	maxLock    time.Duration
}

// NewThrottlePolicy returns a policy. Invalid values fall back to the
// defaults.
func NewThrottlePolicy(delayAfter, lockAfter int, lockFor, maxLock time.Duration) ThrottlePolicy {
	d := DefaultThrottlePolicy()

	if delayAfter > 0 {
		d.delayAfter = delayAfter
	}

	if lockAfter > d.delayAfter {
		d.lockAfter = lockAfter
	}

	if lockFor > 0 {
		d.lockFor = lockFor
	}

	if maxLock >= d.lockFor {
		d.maxLock = maxLock
	}

	return d
}

// DefaultThrottlePolicy: delay from the 5th failure, lock for 15 minutes at
// the 10th, at most 24 hours.
func DefaultThrottlePolicy() ThrottlePolicy {
	return ThrottlePolicy{delayAfter: 5, lockAfter: 10, lockFor: 15 * time.Minute, maxLock: 24 * time.Hour}
}

// BlockedUntil returns when the next attempt is allowed after the given
// number of consecutive failures, or the zero time when it is allowed now.
func (p ThrottlePolicy) BlockedUntil(failures int, now time.Time) time.Time {
	switch {
	case failures < p.delayAfter:
		return time.Time{}
	case failures < p.lockAfter:
		return now.Add(time.Second << (failures - p.delayAfter))
	}

	d := p.lockFor

	for range failures - p.lockAfter {
		if d >= p.maxLock {
			break
		}

		d *= 2
	}

	return now.Add(min(d, p.maxLock))
}

// Locks reports whether that many failures lock the account (audited as a
// lock-out), rather than only delaying the next attempt.
func (p ThrottlePolicy) Locks(failures int) bool { return failures >= p.lockAfter }
