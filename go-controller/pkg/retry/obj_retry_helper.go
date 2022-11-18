package retry

import (
	"time"

	"github.com/onsi/gomega"
	"github.com/onsi/gomega/types"
)

// helper functions to lock RetryFramework properly and inspect retry entry fields in unit tests

const (
	inspectTimeout = 4 * time.Second // arbitrary, to avoid failures on github CI
)

func CheckRetryObj(key string, r *RetryFramework) bool {
	r.retryEntries.LockKey(key)
	defer r.retryEntries.UnlockKey(key)
	_, found := r.getRetryObj(key)
	return found
}

func GetRetryObj(key string, r *RetryFramework) (*retryObjEntry, bool) {
	r.retryEntries.LockKey(key)
	defer r.retryEntries.UnlockKey(key)
	return r.getRetryObj(key)
}

func GetOldObjFromRetryObj(key string, r *RetryFramework) interface{} {
	r.retryEntries.LockKey(key)
	defer r.retryEntries.UnlockKey(key)
	obj, exists := r.getRetryObj(key)
	if exists && obj != nil {
		return obj.oldObj
	}
	return nil
}

func GetNewObjFieldFromRetryObj(key string, r *RetryFramework) interface{} {
	r.retryEntries.LockKey(key)
	defer r.retryEntries.UnlockKey(key)
	obj, exists := r.getRetryObj(key)
	if exists && obj != nil {
		return obj.newObj
	}
	return nil
}

func SetNewObjFieldInRetryObj(key string, r *RetryFramework, newObj interface{}) bool {
	r.retryEntries.LockKey(key)
	defer r.retryEntries.UnlockKey(key)
	obj, exists := r.getRetryObj(key)
	if exists && obj != nil {
		obj.newObj = newObj
		return true
	}
	return false
}

func GetConfigFromRetryObj(key string, r *RetryFramework) interface{} {
	r.retryEntries.LockKey(key)
	defer r.retryEntries.UnlockKey(key)
	obj, exists := r.getRetryObj(key)
	if exists && obj != nil {
		return obj.config
	}
	return nil
}

func SetFailedAttemptsCounterForTestingOnly(key string, val uint8, r *RetryFramework) {
	r.DoWithLock(key, func(key string) {
		entry, found := r.getRetryObj(key)
		if found {
			entry.failedAttempts = val
		}
	})
}

func SetRetryObjWithNoBackoff(key string, r *RetryFramework) {
	r.DoWithLock(key, func(key string) {
		entry, found := r.getRetryObj(key)
		if found {
			r.setRetryObjWithNoBackoff(entry)
		}
	})
}

func InitRetryObjWithAdd(obj interface{}, key string, r *RetryFramework) {
	r.DoWithLock(key, func(key string) {
		r.initRetryObjWithAdd(obj, key)
	})
}

func DeleteRetryObj(key string, r *RetryFramework) {
	r.DoWithLock(key, func(key string) {
		r.DeleteRetryObj(key)
	})
}

func CheckRetryObjectEventually(key string, shouldExist bool, r *RetryFramework) {
	expectedValue := gomega.BeTrue()
	if !shouldExist {
		expectedValue = gomega.BeFalse()
	}
	gomega.Eventually(func() bool {
		return CheckRetryObj(key, r)
	}, inspectTimeout).Should(expectedValue)
}

// same as CheckRetryObjectEventually, but takes an input gomega argument from which
// the assertion is made. This is to be used from within an Eventually block.
func CheckRetryObjectEventuallyWrapped(g gomega.Gomega, key string, shouldExist bool, r *RetryFramework) {
	expectedValue := gomega.BeTrue()
	if !shouldExist {
		expectedValue = gomega.BeFalse()
	}
	g.Eventually(func() bool {
		return CheckRetryObj(key, r)
	}, inspectTimeout).Should(expectedValue)
}

// CheckRetryObjectMultipleFieldsEventually verifies that eventually the oldObj, newObj, config and
// failedAttemptsfields fields all satisfy the input conditions expectedParams, given in
// the same order. In order not to check any of these four fields, the corresponding input
// type.GomegaMatcher must be set to nil.
func CheckRetryObjectMultipleFieldsEventually(
	key string,
	r *RetryFramework,
	expectedParams ...types.GomegaMatcher) {

	var expectedNewObj, expectedOldObj, expectedConfig, expectedFailedAttempts types.GomegaMatcher
	for i, expectedParam := range expectedParams {
		switch i {
		case 0:
			expectedOldObj = expectedParam
		case 1:
			expectedNewObj = expectedParam
		case 2:
			expectedConfig = expectedParam
		case 3:
			expectedFailedAttempts = expectedParam
		}
	}

	gomega.Eventually(func(g gomega.Gomega) {
		r.DoWithLock(key, func(key string) {
			obj, exists := r.getRetryObj(key)
			g.Expect(exists).To(gomega.BeTrue())
			g.Expect(obj).NotTo(gomega.BeNil())

			if exists {
				if expectedOldObj != nil {
					g.Expect(obj.oldObj).To(expectedOldObj)
				}
				if expectedNewObj != nil {
					g.Expect(obj.newObj).To(expectedNewObj)
				}
				if expectedConfig != nil {
					g.Expect(obj.config).To(expectedConfig)
				}
				if expectedFailedAttempts != nil {
					g.Expect(obj.failedAttempts).To(expectedFailedAttempts)
				}
			}
		})
	}, inspectTimeout).Should(gomega.Succeed())
}

func RetryObjsLen(r *RetryFramework) int {
	return len(r.retryEntries.GetKeys())
}
