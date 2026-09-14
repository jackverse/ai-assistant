package scan

import (
	"context"
	"sync"
)

// task 是一个待处理的目录。
type task struct {
	path  string
	depth int

	// retain 表示该目录的最终统计是否需要保留到结果集（用于报告）。
	// 无论是否保留，节点都必须存在，因为父目录的总量依赖子目录回传。
	retain bool

	// retainChildren 表示该目录的子目录是否也值得保留。
	// SummaryOnly 目录（如 WinSxS）会把它置为 false。
	retainChildren bool
}

// workQueue 是一个无界工作队列。
//
// 为什么用无界队列而不是有界 channel：worker 在枚举过程中既要「入队子目录」
// 又要「回传自身完成事件」。如果入队在队列满时阻塞，而所有 worker 都在入队，
// 就没有人消费队列，直接死锁。无界队列消除了这个死锁可能。
// 内存代价可接受：任务只含一个路径字符串，15 万目录约数十 MB。
//
// 使用 sync.Cond 而非 channel：Cond 的原生语义（Wait/Signal）能避免
// 有界 channel 的缓冲区丢失唤醒问题，配合 ctx 取消广播后行为确定。
type workQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []task
	closed bool
}

func newWorkQueue() *workQueue {
	q := &workQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// push 入队一个任务。使用 Broadcast 而非 Signal：
// worker 数量不超过 32，惊群代价可忽略，但能杜绝「信号被一个 worker 消费、
// 其余 worker 仍在睡眠而队列非空」造成的停滞。
func (q *workQueue) push(t task) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.items = append(q.items, t)
	q.mu.Unlock()
	q.cond.Broadcast()
}

// pop 取一个任务。队列空且未关闭时阻塞等待；ctx 取消时返回 false。
func (q *workQueue) pop(ctx context.Context) (task, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for len(q.items) == 0 {
		if q.closed {
			return task{}, false
		}
		if ctx.Err() != nil {
			return task{}, false
		}
		q.cond.Wait()
	}

	// 后进先出：深度优先能更快地完成一整棵子树，
	// 让自底向上的聚合更早释放内存。
	n := len(q.items) - 1
	t := q.items[n]
	q.items[n] = task{}
	q.items = q.items[:n]
	return t, true
}

// close 唤醒所有等待者并阻止后续入队。ctx 取消时由专门的 goroutine 调用。
func (q *workQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.cond.Broadcast()
}

// watchCancel 在 ctx 取消时关闭队列，让阻塞在 pop 上的 worker 退出。
func (q *workQueue) watchCancel(ctx context.Context) {
	go func() {
		<-ctx.Done()
		q.close()
	}()
}
