package commerce

import "sync"

// 生效中的支付通道集合。
//
// 原来是启动时拍一张快照、之后只读（reconciliation.go 里那句「channels 必须保持
// 不可变」就是当时的约定）。运营方在面板上改配置之后要立刻生效，于是这里改成
// 一个带锁的当前值：读的地方取一次快照，写的地方整体替换。通道最多五条，复制
// 一张表的代价可以忽略，换来的是不用在每个读取点上都提心吊胆。
type channelSet struct {
	mu sync.RWMutex
	m  map[string]Channel
}

// SetChannels 替换当前生效的通道集合。传进来的表会被复制：调用方之后改自己
// 那份不会影响已经生效的配置。
func (s *Service) SetChannels(next map[string]Channel) {
	copied := make(map[string]Channel, len(next))
	for name, channel := range next {
		copied[name] = channel
	}
	s.channelSet.mu.Lock()
	s.channelSet.m = copied
	s.channelSet.mu.Unlock()
}

// Channels 返回当前生效的通道集合。只读：需要改就再调一次 SetChannels。
func (s *Service) Channels() map[string]Channel {
	s.channelSet.mu.RLock()
	defer s.channelSet.mu.RUnlock()
	return s.channelSet.m
}

func (s *Service) channel(name string) (Channel, bool) {
	s.channelSet.mu.RLock()
	defer s.channelSet.mu.RUnlock()
	channel, ok := s.channelSet.m[name]
	return channel, ok
}
