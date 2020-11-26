// -*- Mode: Go; indent-tabs-mode: t -*-
// AMS - Anbox Management Service
// Copyright 2018 Canonical Ltd.  All rights reserved.

package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/gorilla/websocket"

	"github.com/anbox-cloud/ams-sdk/shared/rest/api"
)

// Operation wrapper type for operations response allowing certain additional logic
// like blocking current thread until operations completes or cancel it.
type operation struct {
	api.Operation

	c            *operations
	listener     *EventListener
	handlerReady bool
	handlerLock  sync.Mutex

	chActive chan bool
}

// AddHandler adds a function to be called whenever an event is received
func (op *operation) AddHandler(function func(api.Operation)) (*EventTarget, error) {
	// Make sure we have a listener setup
	err := op.setupListener()
	if err != nil {
		return nil, err
	}

	// Make sure we're not racing with ourselves
	op.handlerLock.Lock()
	defer op.handlerLock.Unlock()

	// If we're done already, just return
	if op.StatusCode.IsFinal() {
		return nil, nil
	}

	// Wrap the function to filter unwanted messages
	wrapped := func(data interface{}) {
		newOp := op.extractOperation(data)
		if newOp == nil {
			return
		}

		function(*newOp)
	}

	return op.listener.AddHandler([]string{"operation"}, wrapped)
}

// Cancel will request that server cancels the operation (if supported)
func (op *operation) Cancel() error {
	return op.c.DeleteOperation(op.ID)
}

// Get returns the API operation struct
func (op *operation) Get() api.Operation {
	return op.Operation
}

// GetWebsocket returns a raw websocket connection from the operation
func (op *operation) GetWebsocket() (*websocket.Conn, error) {
	return op.c.GetOperationWebsocket(op.ID)
}

// RemoveHandler removes a function to be called whenever an event is received
func (op *operation) RemoveHandler(target *EventTarget) error {
	// Make sure we're not racing with ourselves
	op.handlerLock.Lock()
	defer op.handlerLock.Unlock()

	// If the listener is gone, just return
	if op.listener == nil {
		return nil
	}

	return op.listener.RemoveHandler(target)
}

// Refresh pulls the current version of the operation and updates the struct
func (op *operation) Refresh() error {
	// Don't bother with a manual update if we are listening for events
	if op.handlerReady {
		return nil
	}

	// Get the current version of the operation
	newOp, _, err := op.c.RetrieveOperationByID(op.ID)
	if err != nil {
		return err
	}

	// Update the operation struct
	op.Operation = *newOp

	return nil
}

// Wait lets you wait until the operation reaches a final state
func (op *operation) Wait(ctx context.Context) error {
	// Check if not done already
	if op.StatusCode.IsFinal() {
		if op.Err != "" {
			return fmt.Errorf(op.Err)
		}
		return nil
	}

	// Make sure we have a listener setup
	err := op.setupListener()
	if err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		if err := op.Cancel(); err != nil {
			return fmt.Errorf("Cannot cancel operation %v: %v", op.ID, err)
		}
		switch ctx.Err() {
		case context.Canceled:
			return errors.New("Operation cancelled")
		case context.DeadlineExceeded:
			return errors.New("Operation timeout")
		default:
			return ctx.Err()
		}
	case <-op.chActive:
	}

	// We're done, parse the result
	if op.Err != "" {
		return fmt.Errorf(op.Err)
	}
	return nil
}

func (op *operation) setupListener() error {
	// Make sure we're not racing with ourselves
	op.handlerLock.Lock()
	defer op.handlerLock.Unlock()

	// We already have a listener setup
	if op.handlerReady {
		return nil
	}

	// Get a new listener
	if op.listener == nil {
		listener, err := op.c.GetEvents()
		if err != nil {
			return err
		}

		op.listener = listener
	}

	// Setup the handler
	chReady := make(chan bool)
	_, err := op.listener.AddHandler([]string{"operation"}, func(data interface{}) {
		<-chReady

		// Get an operation struct out of this data
		newOp := op.extractOperation(data)
		if newOp == nil {
			return
		}

		// We don't want concurrency while processing events
		op.handlerLock.Lock()
		defer op.handlerLock.Unlock()

		// Check if we're done already (because of another event)
		if op.listener == nil {
			return
		}

		// Update the struct
		op.Operation = *newOp

		// And check if we're done
		if op.StatusCode.IsFinal() {
			op.listener.Disconnect()
			op.listener = nil
			close(op.chActive)
			return
		}
	})
	if err != nil {
		op.listener.Disconnect()
		op.listener = nil
		close(op.chActive)
		close(chReady)

		return err
	}

	// Monitor event listener
	go func() {
		<-chReady

		// We don't want concurrency while accessing the listener
		op.handlerLock.Lock()

		// Check if we're done already (because of another event)
		listener := op.listener
		if listener == nil {
			op.handlerLock.Unlock()
			return
		}
		op.handlerLock.Unlock()

		// Wait for the listener or operation to be done
		select {
		case <-listener.chActive:
			op.handlerLock.Lock()
			if op.listener != nil {
				op.Err = fmt.Sprintf("%v", listener.err)
				close(op.chActive)
			}
			op.handlerLock.Unlock()
		case <-op.chActive:
			return
		}
	}()

	// And do a manual refresh to avoid races
	err = op.Refresh()
	if err != nil {
		op.listener.Disconnect()
		op.listener = nil
		close(op.chActive)
		close(chReady)

		return err
	}

	// Check if not done already
	if op.StatusCode.IsFinal() {
		op.listener.Disconnect()
		op.listener = nil
		close(op.chActive)

		op.handlerReady = true
		close(chReady)

		if op.Err != "" {
			return fmt.Errorf(op.Err)
		}

		return nil
	}

	// Start processing background updates
	op.handlerReady = true
	close(chReady)

	return nil
}

func (op *operation) extractOperation(data interface{}) *api.Operation {
	// Extract the metadata
	meta, ok := data.(map[string]interface{})["metadata"]
	if !ok {
		return nil
	}

	// And attempt to decode it as JSON operation data
	encoded, err := json.Marshal(meta)
	if err != nil {
		return nil
	}

	newOp := api.Operation{}
	err = json.Unmarshal(encoded, &newOp)
	if err != nil {
		return nil
	}

	// And now check that it's what we want
	if newOp.ID != op.ID {
		return nil
	}

	return &newOp
}
