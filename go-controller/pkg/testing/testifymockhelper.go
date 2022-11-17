package testing

import (
	"github.com/stretchr/testify/mock"
)

// TestifyMockHelper captures the arguments needed for the `On` , `Return` and 'Times` method ( refer
// https://godoc.org/github.com/stretchr/testify/mock#Call.On ,
// https://godoc.org/github.com/stretchr/testify/mock#Call.Return and
// https://godoc.org/github.com/stretchr/testify/mock#Call.Times )
type TestifyMockHelper struct {
	// OnCallMethodName - mock method that will be called.
	// Refer the `Method` field at https://godoc.org/github.com/stretchr/testify/mock#Call
	OnCallMethodName string
	// OnCallMethodArgType - argument types of the method that will be called.
	// Refer the `Arguments` field at https://godoc.org/github.com/stretchr/testify/mock#Call
	OnCallMethodArgType []string
	// OnCallMethodArgs - argument values we expect to be passed to the method.
	// Refer the `Arguments` field at https://godoc.org/github.com/stretchr/testify/mock#Call
	OnCallMethodArgs []interface{}
	// RetArgList - arguments returned by mock method when called.
	// Refer the `ReturnArguments` field at https://godoc.org/github.com/stretchr/testify/mock#Call
	RetArgList []interface{}
	// OnCallMethodsArgsStrTypeAppendCount - number of times the `string` type argument is repeated at the end.
	// NOTE: There are a couple of cases where the ovn related calls take upto 23 string arguments
	OnCallMethodsArgsStrTypeAppendCount int
	// CallTimes - number of times to return the return arguments when mock method is called
	// Refer the `Repeatability` field at https://godoc.org/github.com/stretchr/testify/mock#Call
	// NOTE: The zero value for an int is 0 in go, however, it the value for 'CallTimes' is not set, we interpret that
	// it will need to be called once.
	CallTimes int
}

// ProcessMockFnList allows for handling mocking of multiple/differnt method calls on a single mock object
func ProcessMockFnList(mockObj *mock.Mock, mArgs []TestifyMockHelper) {
	for _, item := range mArgs {
		ProcessMockFn(mockObj, item)
	}
}

// ProcessMockFn handles mocking of a single method call on a single mock object
func ProcessMockFn(mockObj *mock.Mock, mArgs TestifyMockHelper) {
	if mockObj == nil {
		panic("mock object missing")
	}
	call := mockObj.On(mArgs.OnCallMethodName)
	if len(mArgs.OnCallMethodArgs) > 0 {
		call.Arguments = mArgs.OnCallMethodArgs
	} else {
		for _, arg := range mArgs.OnCallMethodArgType {
			call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
		}
		// append the repetitive arg types of `string` at the end
		for i := 0; i < mArgs.OnCallMethodsArgsStrTypeAppendCount; i++ {
			call.Arguments = append(call.Arguments, mock.AnythingOfType("string"))
		}
	}
	for _, ret := range mArgs.RetArgList {
		call.ReturnArguments = append(call.ReturnArguments, ret)
	}
	// set the default to once if no input is provided.
	if mArgs.CallTimes == 0 {
		call.Once()
	} else {
		call.Times(mArgs.CallTimes)
	}
}
