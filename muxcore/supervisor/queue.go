package supervisor

import "time"

type pendingFrame struct {
	sequence           uint64
	arrived            time.Time
	frame              *parsedFrame
	sameGenerationOnly bool
}

type pendingQueue struct {
	items []*pendingFrame
	bytes int
}

func (queue *pendingQueue) enqueue(item *pendingFrame, maxFrames, maxBytes int) bool {
	size := len(item.frame.raw) + 1
	if len(queue.items) >= maxFrames || size > maxBytes-queue.bytes {
		return false
	}
	queue.items = append(queue.items, item)
	queue.bytes += size
	return true
}

func (queue *pendingQueue) pushFrontBounded(item *pendingFrame, maxFrames, maxBytes int) bool {
	size := len(item.frame.raw) + 1
	if len(queue.items) >= maxFrames || size > maxBytes-queue.bytes {
		return false
	}
	queue.items = append(queue.items, nil)
	copy(queue.items[1:], queue.items)
	queue.items[0] = item
	queue.bytes += size
	return true
}

func (queue *pendingQueue) pop() *pendingFrame {
	if len(queue.items) == 0 {
		return nil
	}
	item := queue.items[0]
	copy(queue.items, queue.items[1:])
	queue.items[len(queue.items)-1] = nil
	queue.items = queue.items[:len(queue.items)-1]
	queue.bytes -= len(item.frame.raw) + 1
	return item
}

func (queue *pendingQueue) popFirstInitializationPing() *pendingFrame {
	for index, item := range queue.items {
		if !isInitializationPing(item.frame) {
			continue
		}
		queue.bytes -= len(item.frame.raw) + 1
		copy(queue.items[index:], queue.items[index+1:])
		queue.items[len(queue.items)-1] = nil
		queue.items = queue.items[:len(queue.items)-1]
		return item
	}
	return nil
}

func (queue *pendingQueue) removeRequest(key scalarKey) bool {
	for index, item := range queue.items {
		frame := item.frame
		if frame.kind != frameRequest || frame.id == nil || frame.id.key != key {
			continue
		}
		if frame.method == "initialize" || frame.taskAugmented {
			return false
		}
		queue.bytes -= len(frame.raw) + 1
		copy(queue.items[index:], queue.items[index+1:])
		queue.items[len(queue.items)-1] = nil
		queue.items = queue.items[:len(queue.items)-1]
		return true
	}
	return false
}

func (queue *pendingQueue) containsRequest(key scalarKey) bool {
	for _, item := range queue.items {
		if item.frame.kind == frameRequest && item.frame.id != nil && item.frame.id.key == key {
			return true
		}
	}
	return false
}

func (queue *pendingQueue) containsProgressToken(key scalarKey) bool {
	for _, item := range queue.items {
		if item.frame.kind == frameRequest && item.frame.progressToken != nil && item.frame.progressToken.key == key {
			return true
		}
	}
	return false
}

func (queue *pendingQueue) discardSameGenerationOnly() {
	kept := queue.items[:0]
	for _, item := range queue.items {
		if item.sameGenerationOnly {
			queue.bytes -= len(item.frame.raw) + 1
			continue
		}
		kept = append(kept, item)
	}
	for index := len(kept); index < len(queue.items); index++ {
		queue.items[index] = nil
	}
	queue.items = kept
}

func (queue *pendingQueue) clear() {
	for index := range queue.items {
		queue.items[index] = nil
	}
	queue.items = nil
	queue.bytes = 0
}

func (queue *pendingQueue) len() int { return len(queue.items) }
