package cache

import (
	"io/ioutil"
	"net"
	"strings"
)

// RamSet 在go内存中的ipset
type RamSet struct {
	subnet []*net.IPNet
	ipMap  map[string]bool
}

// Contain 判断目标ip是否在范围内
func (s *RamSet) Contain(target net.IP) bool {
	if _, ok := s.ipMap[target.String()]; ok {
		return true
	}
	for _, subnet := range s.subnet {
		if subnet.Contains(target) {
			return true
		}
	}
	return false
}

// NewRamSetByText 用文本内容初始化一个RamSet，每行一个ip/网段
func NewRamSetByText(text string) (s *RamSet) {
	s = &RamSet{subnet: []*net.IPNet{}, ipMap: map[string]bool{}}
	for _, line := range strings.Split(text, "\n") {
		line = strings.Trim(line, " \t\n\r")
		s.ipMap[net.ParseIP(line).String()] = true
		if _, subnet, err := net.ParseCIDR(line); err == nil {
			s.subnet = append(s.subnet, subnet)
		}
	}
	return s
}

// NewRamSetByFile 用文件内容初始化一个RamSet，每行一个ip/网段
func NewRamSetByFile(filename string) (matcher *RamSet, err error) {
	var raw []byte
	if raw, err = ioutil.ReadFile(filename); err != nil {
		return nil, err
	}
	return NewRamSetByText(string(raw)), nil
}
