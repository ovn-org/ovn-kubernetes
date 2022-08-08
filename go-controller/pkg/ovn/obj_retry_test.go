package ovn

import (
	"github.com/onsi/gomega"
)

// help functions to lock retryObj properly
func checkRetryObj(key string, r *RetryObjs) bool {
	r.retryEntries.LockKey(key)
	defer r.retryEntries.UnlockKey(key)
	_, found := r.getRetryObj(key)
	return found
}

func getRetryObj(key string, r *RetryObjs) (*retryObjEntry, bool) {
	r.retryEntries.LockKey(key)
	defer r.retryEntries.UnlockKey(key)
	return r.getRetryObj(key)
}

func setFailedAttemptsCounterForTestingOnly(key string, val uint8, r *RetryObjs) {
	r.DoWithLock(key, func(key string) {
		entry, found := r.getRetryObj(key)
		if found {
			entry.failedAttempts = val
		}
	})
}

func setRetryObjWithNoBackoff(key string, r *RetryObjs) {
	r.DoWithLock(key, func(key string) {
		entry, found := r.getRetryObj(key)
		if found {
			r.setRetryObjWithNoBackoff(entry)
		}
	})
}

func initRetryObjWithAdd(obj interface{}, key string, r *RetryObjs) {
	r.DoWithLock(key, func(key string) {
		r.initRetryObjWithAdd(obj, key)
	})
}

func deleteRetryObj(key string, r *RetryObjs) {
	r.DoWithLock(key, func(key string) {
		r.deleteRetryObj(key)
	})
}

func checkRetryObjectEventually(key string, shouldExist bool, r *RetryObjs) {
	expectedValue := gomega.BeTrue()
	if !shouldExist {
		expectedValue = gomega.BeFalse()
	}
	gomega.Eventually(func() bool {
		return checkRetryObj(key, r)
	}).Should(expectedValue)
}

func retryObjsLen(r *RetryObjs) int {
	return len(r.retryEntries.GetKeys())
}
