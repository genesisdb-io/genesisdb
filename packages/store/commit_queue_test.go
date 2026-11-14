package store

import "testing"

func TestCommitQueue_WithdrawClearsUnusedSlots(t *testing.T) {
	for _, order := range []struct {
		name      string
		positions []int
	}{
		{name: "newest first", positions: []int{3, 2, 1, 0}},
		{name: "oldest first", positions: []int{0, 1, 2, 3}},
		{name: "middle first", positions: []int{1, 2, 0, 3}},
	} {
		t.Run(order.name, func(t *testing.T) {
			var queue commitQueue
			members := []*pendingCommit{{}, {}, {}, {}}
			for _, member := range members {
				queue.enqueue(member)
			}
			backing := queue.members[:cap(queue.members)]
			withdrawn := make(map[*pendingCommit]bool)
			for _, position := range order.positions {
				member := members[position]
				if !queue.withdraw(member) {
					t.Fatal("unclaimed commit could not be withdrawn")
				}
				withdrawn[member] = true
				next := 0
				for _, remaining := range members {
					if withdrawn[remaining] {
						continue
					}
					if next >= len(queue.members) || queue.members[next] != remaining {
						t.Fatal("withdrawal changed the order of remaining commits")
					}
					next++
				}
				if len(queue.members) != next {
					t.Fatalf("queue length = %d, want %d", len(queue.members), next)
				}
				for index, retained := range backing[len(queue.members):] {
					if retained != nil {
						t.Fatalf("unused backing slot %d retains a commit", len(queue.members)+index)
					}
				}
			}
		})
	}
}
