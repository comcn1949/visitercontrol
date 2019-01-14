package visitercontrol

import (
	"github.com/yudeguang/hashset"
	"sync"
	"time"
)

//某单位时间内允许多少次访问
type Visitercontrol struct {
	defaultExpiration          time.Duration       //每条访问记录需要保存的时长，也就是过期时间
	cleanupInterval            time.Duration       //多长时间需要执行一次清除操作
	maxVisitsNum               int                 //每个用户在相应时间段内最多允许访问的次数
	indexes                    sync.Map            //索引：key代表用户名或IP；value代表visitorRecords中的索引位置
	maximumNumberOfOnlineUsers int                 //单位时间最大用户数量，建议选用一个稍大于实际值的值，以减少内存分配次数
	visitorRecords             []*circleQueueInt64 //存储用户访问记录
	notUsedVisitorRecordsIndex *hashset.SetInt     //对应visitorRecords中未使用的数据的索引位置
	lock                       *sync.RWMutex       //并发锁
}

/*
初始化
例：
vc := visitercontrol.New(time.Minute*30, time.Second*5, 50, 1000)
它表示:
在30分钟内每个用户最多允许访问50次，系统每5秒针删除一次过期数据。
并且我们预计同时在线用户数量大致在1000个左右。
*/
func New(defaultExpiration, cleanupInterval time.Duration, maxVisitsNum, maximumNumberOfOnlineUsers int) *Visitercontrol {
	this := new(defaultExpiration, cleanupInterval, maxVisitsNum, maximumNumberOfOnlineUsers)
	go this.deleteExpired()
	return this
}

func new(defaultExpiration, cleanupInterval time.Duration, maxVisitsNum, maximumNumberOfOnlineUsers int) *Visitercontrol {
	if cleanupInterval > defaultExpiration {
		panic("每次清除访问记录的时间间隔(cleanupInterval)必须小于待统计数据时间段(defaultExpiration)")
	}
	var l Visitercontrol
	var lock sync.RWMutex
	l.defaultExpiration = defaultExpiration
	l.cleanupInterval = cleanupInterval
	l.maxVisitsNum = maxVisitsNum
	l.maximumNumberOfOnlineUsers = maximumNumberOfOnlineUsers
	l.notUsedVisitorRecordsIndex = hashset.NewInt()
	l.lock = &lock
	//初始化缓存池，减少内存分配，提升性能
	l.visitorRecords = make([]*circleQueueInt64, l.maximumNumberOfOnlineUsers)
	for i := range l.visitorRecords {
		l.visitorRecords[i] = newCircleQueueInt64(l.maxVisitsNum)
		l.notUsedVisitorRecordsIndex.Add(i)
	}
	return &l

}

//是否允许访问,允许访问则往访问记录中加入一条访问记录
func (this *Visitercontrol) AllowVisit(key interface{}) bool {
	return this.add(key) == nil
}

//是否允许某IP的用户访问
func (this *Visitercontrol) AllowVisitIP(ip string) bool {
	ipInt64 := this.Ip4StringToInt64(ip)
	if ipInt64 == 0 {
		return false
	}
	return this.AllowVisit(ipInt64)
}

//增加一条访问记录
func (this *Visitercontrol) add(key interface{}) (err error) {
	index, exist := this.indexes.Load(key)
	//存在某访客，则在该访客记录中增加一条访问记录
	if exist {
		return this.visitorRecords[index.(int)].Push(time.Now().Add(this.defaultExpiration).UnixNano())
	} else {
		//不存在该访客记录的时候
		this.lock.RLock()
		defer this.lock.RUnlock()
		//有未使用的缓存时
		if this.notUsedVisitorRecordsIndex.Size() > 0 {
			for index := range this.notUsedVisitorRecordsIndex.Items {
				this.visitorRecords[index].Push(time.Now().Add(this.defaultExpiration).UnixNano())
				this.notUsedVisitorRecordsIndex.Remove(index)
				//下标索引位置
				this.indexes.Store(key, index)
				break
			}

		} else {
			//没有缓存可使用时
			queue := newCircleQueueInt64(this.maxVisitsNum)
			queue.Push(time.Now().Add(this.defaultExpiration).UnixNano())
			this.visitorRecords = append(this.visitorRecords, queue)
			//最后一条数据是下标索引位置
			this.indexes.Store(key, len(this.visitorRecords)-1)
		}
		return nil
	}
}

//删除过期数据
func (this *Visitercontrol) deleteExpired() {
	finished := true
	for range time.Tick(this.cleanupInterval) {
		//如果数据量较大，那么在一个清除周期内不一定会把所有数据全部清除
		if finished {
			finished = false
			this.deleteExpiredOnce()
			this.gc()
			finished = true
		}
	}
}

//在特定时间间隔内执行一次删除过期数据操作
func (this *Visitercontrol) deleteExpiredOnce() {
	this.indexes.Range(func(k, v interface{}) bool {
		index := v.(int)
		//防止越界出错，理论上不存在这种情况
		if index < len(this.visitorRecords) && index >= 0 {
			this.visitorRecords[index].DeleteExpired()
			//某用户某段时间无访问记录时，删除该用户，并把剩余的空访问记录加入缓存记录池
			if this.visitorRecords[index].Size() == 0 {
				this.lock.Lock()
				defer this.lock.Unlock()
				this.indexes.Delete(k)
				this.notUsedVisitorRecordsIndex.Add(index)
			}
		} else {
			this.indexes.Delete(k)
		}

		return true
	})
}

//把Int64转换成IP4的的字符串形式
func (this *Visitercontrol) Int64ToIp4String(ip int64) string {
	return Int64ToIp4String(ip)
}

//IP4地址转换为Int64
func (this *Visitercontrol) Ip4StringToInt64(ip string) int64 {
	return Ip4StringToInt64(ip)
}

//出现峰值之后，回收访问数据，减少内存占用
func (this *Visitercontrol) gc() {
	this.lock.Lock()
	defer this.lock.Unlock()
	if this.needGc() {
		curLen := len(this.visitorRecords)
		unUsedLen := len(this.notUsedVisitorRecordsIndex.Items)
		usedLen := curLen - unUsedLen
		var newLen int
		if usedLen < this.maximumNumberOfOnlineUsers {
			newLen = this.maximumNumberOfOnlineUsers
		} else {
			newLen = usedLen * 2
		}
		//建立新缓存
		visitorRecordsNew := make([]*circleQueueInt64, newLen)
		for i := range visitorRecordsNew {
			visitorRecordsNew[i] = newCircleQueueInt64(this.maxVisitsNum)
		}
		//清空未使用索引
		this.notUsedVisitorRecordsIndex.Clear()
		//重建索引
		indexNew := 0
		this.indexes.Range(func(k, v interface{}) bool {
			indexOld := v.(int)
			visitorRecordsNew[indexNew] = this.visitorRecords[indexOld]
			indexNew++
			return true
		})
		this.visitorRecords = visitorRecordsNew
		//重建未使用索引
		for i := range this.visitorRecords {
			if i >= indexNew {
				this.notUsedVisitorRecordsIndex.Add(i)
			}
		}
	}
}

//是否需要对visitorRecords进行清理
//如果visitorRecords数据空的太多,则需要进行清理操作
//并且长度远大于默认在线用户数量，则需要进行GC操作
func (this *Visitercontrol) needGc() bool {
	curLen := len(this.visitorRecords)
	unUsedLen := len(this.notUsedVisitorRecordsIndex.Items)
	usedLen := curLen - unUsedLen
	//log.Println("总:", curLen, "已用:", usedLen, "未使用:", unUsedLen)
	//比预期的少，我们就不回收了
	if curLen < 2*this.maximumNumberOfOnlineUsers {
		return false
	}
	//未使用的太多，则需要回收
	if usedLen*2 < unUsedLen {
		return true
	}
	return false
}
