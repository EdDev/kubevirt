/*
 * This file is part of the KubeVirt project
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
 * Copyright The KubeVirt Authors.
 *
 */

package testutils

import (
	"fmt"
	"sync"
	"testing"
)

func TestMockWorkQueueWaitWithoutExpectationsReturnsImmediately(t *testing.T) {
	q := NewMockWorkQueue(NewFrozenClockRateLimitingQueue("test"))

	q.Wait()
}

func TestMockWorkQueueWaitBlocksUntilConcurrentAddsAreDone(t *testing.T) {
	const adds = 16

	q := NewMockWorkQueue(NewFrozenClockRateLimitingQueue("test"))
	q.ExpectAdds(adds)

	var started sync.WaitGroup
	started.Add(adds)
	for i := 0; i < adds; i++ {
		go func(item string) {
			started.Done()
			q.Add(item)
		}(fmt.Sprintf("item-%d", i))
	}
	started.Wait()
	q.Wait()

	if queued := q.Len(); queued != adds {
		t.Fatalf("expected %d queued items after Wait returned, got %d", adds, queued)
	}
}
