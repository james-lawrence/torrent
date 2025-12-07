package krpc

func NewEmptyResponse(tid string) Msg {
	return Msg{
		T: tid,
		Y: "r",
	}
}
