package api

import (
	"fmt"
	"github.com/docker/go-plugins-helpers/network"
	"github.com/docker/go-plugins-helpers/sdk"
	"log"
	"net/http"
	"reflect"
	"runtime"
	"strings"
)

type HandlerFunc[T any] func() (T, error)
type RequestDecoder[T any] func(r *http.Request) (T, error)

type CommonHandlerOptions struct {
	Api           string
	IsInitialized func() bool
}

func MakeHandlerWithOutput[TResponse any](
	commonOptions CommonHandlerOptions,
	process HandlerFunc[TResponse],
) http.HandlerFunc {
	method := getFunctionName(process)
	return func(w http.ResponseWriter, r *http.Request) {
		// Log the receipt of the request
		fmt.Printf("[%s] Received %s\n", commonOptions.Api, method)

		var res TResponse
		var err error
		if !commonOptions.IsInitialized() {
			err = fmt.Errorf("driver is not yet initialized")
		} else {
			res, err = process()
		}

		// Log the response and any errors
		fmt.Printf("[%s] %s response: %+v; error: %+v\n", commonOptions.Api, method, res, err)

		// Handle errors
		if err != nil {
			// Assuming network.NewErrorResponse creates a standardized error response
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}

		// Encode the successful response
		sdk.EncodeResponse(w, res, false)
	}
}

func MakeHandlerWithInputAndOutput[TRequest any, TResponse any](
	commonOptions CommonHandlerOptions,
	process func(decodedReq *TRequest) (TResponse, error),
) http.HandlerFunc {
	method := getFunctionName(process)
	return func(w http.ResponseWriter, r *http.Request) {
		decodedReq := new(TRequest)
		err := sdk.DecodeRequest(w, r, decodedReq)
		if err != nil {
			log.Printf("[%s] %s decode request error: %v\n", commonOptions.Api, method, err)
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		fmt.Printf("[%s] Received %s, req: %+v\n", commonOptions.Api, method, decodedReq)

		var res TResponse
		if !commonOptions.IsInitialized() {
			err = fmt.Errorf("driver is not yet initialized")
		} else {
			res, err = process(decodedReq)
		}

		fmt.Printf("[%s] %s response: %+v; error: %+v\n", commonOptions.Api, method, res, err)

		if err != nil {
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}

		sdk.EncodeResponse(w, res, false)
	}
}

func MakeHandlerWithInput[TRequest any](
	commonOptions CommonHandlerOptions,
	process func(decodedReq *TRequest) error,
) http.HandlerFunc {
	method := getFunctionName(process)
	return func(w http.ResponseWriter, r *http.Request) {
		decodedReq := new(TRequest)
		err := sdk.DecodeRequest(w, r, decodedReq)
		if err != nil {
			log.Printf("[%s] %s decode request error: %v\n", commonOptions.Api, method, err)
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		fmt.Printf("[%s] Received %s, req: %+v\n", commonOptions.Api, method, decodedReq)

		if !commonOptions.IsInitialized() {
			err = fmt.Errorf("driver is not yet initialized")
		} else {
			err = process(decodedReq)
		}

		fmt.Printf("[%s] %s response: %+v\n", commonOptions.Api, method, err)

		if err != nil {
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}

		sdk.EncodeResponse(w, struct{}{}, false)
	}
}

func getFunctionName(i interface{}) string {
	// Retrieve the function's pointer
	ptr := reflect.ValueOf(i).Pointer()
	// Get the function object
	fn := runtime.FuncForPC(ptr)
	if fn == nil {
		return "unknown"
	}
	// Get the full function name, including package path
	fullName := fn.Name()
	// Extract only the function name by splitting on the last dot
	// e.g., "main.(*NetworkDriverImpl).GetCapabilities" -> "GetCapabilities"
	parts := strings.Split(fullName, ".")
	if len(parts) == 0 {
		return "unknown"
	}
	return parts[len(parts)-1]
}
