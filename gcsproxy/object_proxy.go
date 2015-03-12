// Copyright 2015 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gcsproxy

import (
	"fmt"
	"io/ioutil"
	"math"
	"os"

	"github.com/jacobsa/gcloud/gcs"
	"golang.org/x/net/context"
	"google.golang.org/cloud/storage"
)

// A view on a particular generation of an object in GCS that allows random
// access reads and writes.
//
// Reads may involve reading from a local cache. Writes are buffered locally
// until the Sync method is called, at which time a new generation of the
// object is created.
//
// This type is not safe for concurrent access. The user must provide external
// synchronization.
type ObjectProxy struct {
	/////////////////////////
	// Dependencies
	/////////////////////////

	bucket gcs.Bucket

	/////////////////////////
	// Constant data
	/////////////////////////

	// The name of the GCS object for which we are a proxy. Might not currently
	// exist in the bucket.
	name string

	/////////////////////////
	// Mutable state
	/////////////////////////

	// The specific generation of the object from which our local state is
	// branched. If we have no local state, the contents of this object are
	// exactly our contents. May be zero if our source is a "doesn't exist"
	// generation.
	srcGeneration uint64

	// The size of the object from which our local state is branched. If
	// srcGeneration is non-zero, this is the size of that generation in GCS.
	//
	// INVARIANT: If srcGeneration == 0, srcSize == 0
	srcSize uint64

	// A local temporary file containing our current contents. When non-nil, this
	// is the authority on our contents. When nil, our contents are defined by
	// the generation identified by srcGeneration.
	localFile *os.File

	// false if localFile is present but its contents may be different from the
	// contents of our source generation. Sync needs to do work iff this is true.
	//
	// INVARIANT: If srcGeneration == 0, then dirty
	// INVARIANT: If dirty, then localFile != nil
	dirty bool
}

////////////////////////////////////////////////////////////////////////
// Public interface
////////////////////////////////////////////////////////////////////////

// Create a view on the given GCS object generation which is assumed to have
// the given size, or zero if branching from a non-existent object (in which
// case the initial contents are empty).
//
// REQUIRES: If srcGeneration == 0, then srcSize == 0
func NewObjectProxy(
	ctx context.Context,
	bucket gcs.Bucket,
	name string,
	srcGeneration uint64,
	srcSize uint64) (op *ObjectProxy, err error) {
	// Set up the basic struct.
	op = &ObjectProxy{
		bucket:        bucket,
		name:          name,
		srcGeneration: srcGeneration,
		srcSize:       srcSize,
	}

	// For "doesn't exist" source generations, we must establish an empty local
	// file and mark the proxy dirty.
	if srcGeneration == 0 {
		if err = op.ensureLocalFile(ctx); err != nil {
			return
		}

		op.dirty = true
	}

	return
}

// Return the name of the proxied object. This may or may not be an object that
// currently exists in the bucket.
func (op *ObjectProxy) Name() string {
	return op.name
}

// Panic if any internal invariants are violated. Careful users can call this
// at appropriate times to help debug weirdness. Consider using
// syncutil.InvariantMutex to automate the process.
func (op *ObjectProxy) CheckInvariants() {
	// INVARIANT: If srcGeneration == 0, srcSize == 0
	if op.srcGeneration == 0 && op.srcSize != 0 {
		panic("Expected zero source size.")
	}

	// INVARIANT: If srcGeneration == 0, then dirty
	if op.srcGeneration == 0 && !op.dirty {
		panic("Expected dirty.")
	}

	// INVARIANT: If dirty, then localFile != nil
	if op.dirty && op.localFile == nil {
		panic("Expected non-nil localFile.")
	}
}

// Destroy any local file caches, putting the proxy into an indeterminate
// state. Should be used before dropping the final reference to the proxy.
func (op *ObjectProxy) Destroy() (err error) {
	// Make sure that when we exit no invariants are violated.
	defer func() {
		op.srcGeneration = 1
		op.localFile = nil
		op.dirty = false
	}()

	// If we have no local file, there's nothing to do.
	if op.localFile == nil {
		return
	}

	// Close the local file.
	if err = op.localFile.Close(); err != nil {
		err = fmt.Errorf("Close: %v", err)
		return
	}

	return
}

// Return the current size in bytes of the content and an indication of whether
// the proxied object has changed out from under us (in which case Sync will
// fail).
func (op *ObjectProxy) Stat(
	ctx context.Context) (size uint64, clobbered bool, err error) {
	panic("TODO")
}

// Make a random access read into our view of the content. May block for
// network access.
//
// Guarantees that err != nil if n < len(buf)
func (op *ObjectProxy) ReadAt(
	ctx context.Context,
	buf []byte,
	offset int64) (n int, err error) {
	// Make sure we have a local file.
	if err = op.ensureLocalFile(ctx); err != nil {
		err = fmt.Errorf("ensureLocalFile: %v", err)
		return
	}

	// Serve the read from the file.
	n, err = op.localFile.ReadAt(buf, offset)

	return
}

// Make a random access write into our view of the content. May block for
// network access. Not guaranteed to be reflected remotely until after Sync is
// called successfully.
//
// Guarantees that err != nil if n < len(buf)
func (op *ObjectProxy) WriteAt(
	ctx context.Context,
	buf []byte,
	offset int64) (n int, err error) {
	// Make sure we have a local file.
	if err = op.ensureLocalFile(ctx); err != nil {
		err = fmt.Errorf("ensureLocalFile: %v", err)
		return
	}

	// TODO(jacobsa): Make sure the dirty flag modification below is tested by
	// removing it and looking for a failure.
	op.dirty = true
	n, err = op.localFile.WriteAt(buf, offset)

	return
}

// Truncate our view of the content to the given number of bytes, extending if
// n is greater than the current size. May block for network access. Not
// guaranteed to be reflected remotely until after Sync is called successfully.
func (op *ObjectProxy) Truncate(ctx context.Context, n uint64) (err error) {
	// Make sure we have a local file.
	if err = op.ensureLocalFile(ctx); err != nil {
		err = fmt.Errorf("ensureLocalFile: %v", err)
		return
	}

	// Convert to signed, which is what os.File wants.
	if n > math.MaxInt64 {
		err = fmt.Errorf("Illegal offset: %v", n)
		return
	}

	// TODO(jacobsa): Make sure the dirty flag modification below is tested by
	// removing it and looking for a failure.
	op.dirty = true
	err = op.localFile.Truncate(int64(n))

	return
}

// If the proxy is dirty due to having been written to or due to having a nil
// source, save its current contents to GCS and return a generation number for
// a generation with exactly those contents. Do so with a precondition such
// that the creation will fail if the source generation is not current. In that
// case, return an error of type *gcs.PreconditionError.
func (op *ObjectProxy) Sync(ctx context.Context) (gen uint64, err error) {
	// Do we need to do anything?
	if !op.dirty {
		gen = op.srcGeneration
		return
	}

	// Seek the file to the start so that it can be used as a reader for its full
	// contents below.
	_, err = op.localFile.Seek(0, 0)
	if err != nil {
		err = fmt.Errorf("Seek: %v", err)
		return
	}

	// Write a new generation of the object with the appropriate contents, using
	// an appropriate precondition.
	signedSrcGeneration := int64(op.srcGeneration)
	req := &gcs.CreateObjectRequest{
		Attrs: storage.ObjectAttrs{
			Name: op.name,
		},
		Contents:               op.localFile,
		GenerationPrecondition: &signedSrcGeneration,
	}

	o, err := op.bucket.CreateObject(ctx, req)

	// Special case: handle precondition errors.
	if _, ok := err.(*gcs.PreconditionError); ok {
		err = &gcs.PreconditionError{
			Err: fmt.Errorf("CreateObject: %v", err),
		}

		return
	}

	// Propagate other errors more directly.
	if err != nil {
		err = fmt.Errorf("CreateObject: %v", err)
		return
	}

	// Make sure the server didn't return a silly generation number.
	//
	// TODO(jacobsa): Push unsigned generation numbers and a guarantee on zero
	// into package gcs, including checking results from the server, and remove
	// this.
	if o.Generation <= 0 {
		err = fmt.Errorf(
			"CreateObject returned invalid generation number: %v",
			o.Generation)

		return
	}

	gen = uint64(o.Generation)

	// Update our state.
	op.srcGeneration = gen
	op.dirty = false

	return
}

////////////////////////////////////////////////////////////////////////
// Helpers
////////////////////////////////////////////////////////////////////////

// Set up an unlinked local temporary file for the given generation of the
// given object. Special case: generation == 0 means an empty file.
func makeLocalFile(
	ctx context.Context,
	bucket gcs.Bucket,
	name string,
	generation uint64) (f *os.File, err error) {
	// Create the file.
	f, err = ioutil.TempFile("", "object_proxy")
	if err != nil {
		err = fmt.Errorf("TempFile: %v", err)
		return
	}

	// Unlink the file so that its inode will be garbage collected when the file
	// is closed.
	if err = os.Remove(f.Name()); err != nil {
		f.Close()
		err = fmt.Errorf("Remove: %v", err)
		return
	}

	// Fetch the object's contents if necessary.
	if generation != 0 {
		panic("TODO")
	}

	return
}

// Ensure that op.localFile is non-nil with an authoritative view of op's
// contents.
func (op *ObjectProxy) ensureLocalFile(ctx context.Context) (err error) {
	// Is there anything to do?
	if op.localFile != nil {
		return
	}

	// Set up the file.
	f, err := makeLocalFile(ctx, op.bucket, op.name, op.srcGeneration)
	if err != nil {
		err = fmt.Errorf("makeLocalFile: %v", err)
		return
	}

	op.localFile = f
	return
}
