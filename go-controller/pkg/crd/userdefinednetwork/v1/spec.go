package v1

func (s *UserDefinedNetworkSpec) GetTopology() NetworkTopology {
	return s.Topology
}

func (s *UserDefinedNetworkSpec) GetLayer3() *Layer3Config {
	return s.Layer3
}

func (s *UserDefinedNetworkSpec) GetLayer2() *Layer2Config {
	return s.Layer2
}

func (s *NetworkSpec) GetTopology() NetworkTopology {
	return s.Topology
}

func (s *NetworkSpec) GetLayer3() *Layer3Config {
	return s.Layer3
}

func (s *NetworkSpec) GetLayer2() *Layer2Config {
	return s.Layer2
}
