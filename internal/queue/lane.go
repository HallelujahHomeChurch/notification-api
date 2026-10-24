package queue

// Lane is chosen from the persisted, validated template and channel, never from caller priority.
type Lane string

const (
	LaneAll           Lane = ""
	LaneTransactional Lane = "transactional"
	LaneBulk          Lane = "bulk"
	LaneWebPush       Lane = "web_push"
)
