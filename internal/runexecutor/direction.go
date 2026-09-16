package runexecutor

func outbound(channel Channel, kind string) bool {
	if channel == Metering {
		return kind == "metering_ack"
	}
	return kind == "execute_step" || kind == "call_permit" || kind == "call_observation_ack"
}

func inbound(channel Channel, kind string) bool {
	if channel == Metering {
		return kind == "metering_report"
	}
	return kind == "call_intent" || kind == "call_observation" || kind == "step_result"
}
