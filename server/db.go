package server

import (
    "github.com/snower/slock/protocol"
    "sync"
    "sync/atomic"
    "time"
)

const TIMEOUT_QUEUE_LENGTH int64 = 0x10
const EXPRIED_QUEUE_LENGTH int64 = 0x10
const TIMEOUT_QUEUE_LENGTH_MASK int64 = 0x0f
const EXPRIED_QUEUE_LENGTH_MASK int64 = 0x0f
const FAST_LOCK_SEG_LENGTH uint64 = 0x800000
const FAST_LOCK_SEG_LENGTH_MASK uint64 = 0x7fffff
const FAST_LOCK_RATE uint64 = 512

type LockDB struct {
    slock                       *SLock
    fast_locks                  [][]*LockManager
    locks                       map[[2]uint64]*LockManager
    timeout_locks               [][]*LockQueue
    expried_locks               [][]*LockQueue
    fast_lock_count             uint64
    resizing_fast_lock_count    uint64
    current_time                int64
    check_timeout_time          int64
    check_expried_time          int64
    glock                       sync.Mutex
    manager_glocks              []*sync.Mutex
    free_lock_managers          []*LockManager
    free_locks                  []*LockQueue
    free_lock_manager_count     int32
    manager_glock_index         int8
    manager_max_glocks          int8
    is_stop                     bool
    state                       protocol.LockDBState
}

func NewLockDB(slock *SLock) *LockDB {
    manager_max_glocks := int8(64)
    manager_glocks := make([]*sync.Mutex, manager_max_glocks)
    free_locks := make([]*LockQueue, manager_max_glocks)
    for i:=int8(0); i< manager_max_glocks; i++{
        manager_glocks[i] = &sync.Mutex{}
        free_locks[i] = NewLockQueue(2, 16, 4096)
    }

    fast_locks := make([][]*LockManager, 128)
    fast_locks[0] = make([]*LockManager, FAST_LOCK_SEG_LENGTH)

    now := time.Now().Unix()
    db := &LockDB{slock, fast_locks, make(map[[2]uint64]*LockManager, 262144), make([][]*LockQueue, TIMEOUT_QUEUE_LENGTH),
    make([][]*LockQueue, EXPRIED_QUEUE_LENGTH), FAST_LOCK_SEG_LENGTH, 0,
    now, now, now, sync.Mutex{},
    manager_glocks, make([]*LockManager, 4194304), free_locks, -1,
    0, manager_max_glocks, false, protocol.LockDBState{}}

    db.ResizeTimeOut()
    db.ResizeExpried()
    go db.UpdateCurrentTime()
    go db.CheckResizingFastLocks()
    go db.CheckTimeOut()
    go db.CheckExpried()
    return db
}

func (self *LockDB) ConvertUint642ToByte16(uint642 [2]uint64) [16]byte {
    return [16]byte{byte(uint642[0]), byte(uint642[0] >> 8), byte(uint642[0] >> 16), byte(uint642[0] >> 24),
        byte(uint642[0] >> 32), byte(uint642[0] >> 40), byte(uint642[0] >> 48), byte(uint642[0] >> 56),
        byte(uint642[1]), byte(uint642[1] >> 8), byte(uint642[1] >> 16), byte(uint642[1] >> 24),
        byte(uint642[1] >> 32), byte(uint642[1] >> 40), byte(uint642[1] >> 48), byte(uint642[1] >> 56)}
}

func (self *LockDB) ResizeTimeOut (){
    for i := int64(0); i < TIMEOUT_QUEUE_LENGTH; i++ {
        self.timeout_locks[i] = make([]*LockQueue, self.manager_max_glocks)
        for j := int8(0); j < self.manager_max_glocks; j++ {
            self.timeout_locks[i][j] = NewLockQueue(4, 16, 4096)
        }
    }
}

func (self *LockDB) ResizeExpried (){
    for i := int64(0); i < EXPRIED_QUEUE_LENGTH; i++ {
        self.expried_locks[i] = make([]*LockQueue, self.manager_max_glocks)
        for j := int8(0); j < self.manager_max_glocks; j++ {
            self.expried_locks[i][j] = NewLockQueue(4, 16, 4096)
        }
    }
}

func (self *LockDB) UpdateCurrentTime(){
    for !self.is_stop {
        time.Sleep(5e8)
        self.current_time = time.Now().Unix()
    }
}

func (self *LockDB) CheckTimeOut(){
    for !self.is_stop {
        time.Sleep(1e9)


        check_timeout_time := self.check_timeout_time

        now := self.current_time
        self.check_timeout_time = now + 1
        for _, manager_glock := range self.manager_glocks {
            manager_glock.Lock()
            manager_glock.Unlock()
        }

        for ; check_timeout_time <= now; {
            go self.CheckTimeTimeOut(check_timeout_time, now)
            check_timeout_time++
        }
    }
}

func (self *LockDB) CheckTimeTimeOut(check_timeout_time int64, now int64) {
    timeout_locks := self.timeout_locks[check_timeout_time % TIMEOUT_QUEUE_LENGTH]
    do_timeout_locks := make([]*Lock, 0)

    for i := int8(0); i < self.manager_max_glocks; i++ {
        self.manager_glocks[i].Lock()

        lock := timeout_locks[i].Pop()
        for ; lock != nil; {
            if !lock.timeouted {
                if lock.timeout_time > now {
                    lock.timeout_checked_count++
                    self.AddTimeOut(lock)
                    lock = timeout_locks[i].Pop()
                    continue
                }

                do_timeout_locks = append(do_timeout_locks, lock)
                lock = timeout_locks[i].Pop()
                continue
            }

            lock_manager := lock.manager
            lock.ref_count--
            if lock.ref_count == 0 {
                lock_manager.FreeLock(lock)
            }

            if lock_manager.ref_count == 0 {
                self.RemoveLockManager(lock_manager)
            }

            lock = timeout_locks[i].Pop()
        }

        self.manager_glocks[i].Unlock()
        timeout_locks[i].Reset()
    }

    for _, lock := range do_timeout_locks {
        self.DoTimeOut(lock)
    }
}

func (self *LockDB) CheckExpried(){
    for !self.is_stop {
        time.Sleep(1e9)

        check_expried_time := self.check_expried_time

        now := self.current_time
        self.check_expried_time = now + 1
        for _, manager_glock := range self.manager_glocks {
            manager_glock.Lock()
            manager_glock.Unlock()
        }

        for ; check_expried_time <= now; {
            go self.CheckTimeExpried(check_expried_time, now)
            check_expried_time++
        }

    }
}

func (self *LockDB) CheckTimeExpried(check_expried_time int64, now int64){
    expried_locks := self.expried_locks[check_expried_time % EXPRIED_QUEUE_LENGTH]
    do_expried_locks := make([]*Lock, 0)

    for i := int8(0); i < self.manager_max_glocks; i++ {
        self.manager_glocks[i].Lock()

        lock := expried_locks[i].Pop()
        for ; lock != nil; {
            if !lock.expried {
                if lock.expried_time > now {
                    lock.expried_checked_count++
                    self.AddExpried(lock)

                    lock = expried_locks[i].Pop()
                    continue
                }

                do_expried_locks = append(do_expried_locks, lock)
                lock = expried_locks[i].Pop()
                continue
            }

            lock_manager := lock.manager
            lock.ref_count--
            if lock.ref_count == 0 {
                lock_manager.FreeLock(lock)
            }

            if lock_manager.ref_count == 0 {
                self.RemoveLockManager(lock_manager)
            }
            lock = expried_locks[i].Pop()
        }
        self.manager_glocks[i].Unlock()
        expried_locks[i].Reset()
    }

    for _, lock := range do_expried_locks {
        self.DoExpried(lock)
    }
}

func (self *LockDB) CheckResizingFastLocks() {
    last_lock_count := self.state.LockCount
    for !self.is_stop {
        last_lock_count = self.state.LockCount
        time.Sleep(1e9)
        if (self.state.LockCount - last_lock_count) * FAST_LOCK_RATE > self.fast_lock_count {
            conflict_lock_count := len(self.locks)

            self.resizing_fast_lock_count = self.fast_lock_count
            fast_lock_count := self.fast_lock_count << 1
            fast_lock_index := self.fast_lock_count / FAST_LOCK_SEG_LENGTH

            for self.fast_lock_count < fast_lock_count {
                self.fast_locks[fast_lock_index] = make([]*LockManager, EXPRIED_QUEUE_LENGTH)
                fast_lock_index++
                self.fast_lock_count += FAST_LOCK_SEG_LENGTH
            }

            fast_lock_base_count := self.fast_lock_count / FAST_LOCK_SEG_LENGTH
            for i := uint64(0); i < fast_lock_base_count; i++ {
                fast_locks := self.fast_locks[i]
                for j, lock_manager := range fast_locks {
                    if lock_manager == nil {
                        continue
                    }

                    if lock_manager.conflict_maped {
                        continue
                    }

                    fast_lock_base_index := (lock_manager.lock_key[1] & self.fast_lock_count) >> 23
                    if fast_lock_base_index != i{
                        self.glock.Lock()
                        if !lock_manager.conflict_maped && !lock_manager.freed {
                            self.fast_locks[i][j] = nil
                            self.fast_locks[fast_lock_base_index][lock_manager.lock_key[1] & FAST_LOCK_SEG_LENGTH_MASK] = lock_manager
                        }
                        self.glock.Unlock()
                    }
                }
            }

            self.glock.Lock()
            conflict_locks := make([]*LockManager, len(self.locks))
            conflict_lock_index := 0

            for lock_key, lock_manager := range self.locks {
                if !lock_manager.conflict_maped {
                    self.fast_locks[(lock_key[1] & self.resizing_fast_lock_count) >> 23][lock_key[1] & FAST_LOCK_SEG_LENGTH_MASK].ref_count--
                }
                conflict_locks[conflict_lock_index] = lock_manager
                conflict_lock_index++
                delete(self.locks, lock_key)
            }

            for _, lock_manager := range conflict_locks {
                if lock_manager.conflict_maped {
                    self.fast_locks[(lock_manager.lock_key[1] & self.resizing_fast_lock_count) >> 23][lock_manager.lock_key[1] & FAST_LOCK_SEG_LENGTH_MASK] = nil
                    lock_manager.conflict_maped = false
                }
            }

            for _, lock_manager := range conflict_locks {
                if lock_manager.ref_count == 0 {
                    lock_manager.freed = true

                    if self.free_lock_manager_count < 4194303 {
                        self.free_lock_manager_count++
                        self.free_lock_managers[self.free_lock_manager_count] = lock_manager
                        self.state.KeyCount--

                        if lock_manager.locks != nil {
                            lock_manager.locks.Reset()
                        }
                        if lock_manager.wait_locks != nil {
                            lock_manager.wait_locks.Reset()
                        }
                    } else {
                        self.state.KeyCount--

                        lock_manager.current_lock = nil
                        lock_manager.locks = nil
                        lock_manager.lock_maps = nil
                        lock_manager.wait_locks = nil
                        lock_manager.free_locks = nil
                    }
                } else {
                    fast_lock_base_index := (lock_manager.lock_key[1] & self.fast_lock_count) >> 23
                    fast_lock_index := lock_manager.lock_key[1] & FAST_LOCK_SEG_LENGTH_MASK
                    fast_lock_manager := self.fast_locks[fast_lock_base_index][fast_lock_index]
                    if fast_lock_manager == nil {
                        self.fast_locks[fast_lock_base_index][fast_lock_index] = lock_manager
                    } else {
                        if !fast_lock_manager.conflict_maped {
                            fast_lock_manager.conflict_maped = true
                            self.locks[fast_lock_manager.lock_key] = fast_lock_manager
                        }
                        fast_lock_manager.ref_count++
                        self.locks[lock_manager.lock_key] = lock_manager
                    }
                }
            }

            self.resizing_fast_lock_count = 0
            self.glock.Unlock()
            self.slock.Log().Infof("fast lock resizing %d %d %d", self.fast_lock_count, conflict_lock_count, len(self.locks))
        }
    }
}


func (self *LockDB) GetOrNewLockManager(command *protocol.LockCommand) *LockManager{
    self.glock.Lock()

    fast_lock_base_index := (command.LockKey[1] & self.fast_lock_count) >> 23
    fast_lock_index := command.LockKey[1] & FAST_LOCK_SEG_LENGTH_MASK

    fast_lock_manager := self.fast_locks[fast_lock_base_index][fast_lock_index]
    if fast_lock_manager != nil {
        if !fast_lock_manager.conflict_maped {
            self.glock.Unlock()
            return fast_lock_manager
        }
    } else if self.resizing_fast_lock_count != 0 {
        resizing_fast_lock_manager := self.fast_locks[(command.LockKey[1] & self.resizing_fast_lock_count) >> 23][command.LockKey[1] & FAST_LOCK_SEG_LENGTH_MASK]
        if resizing_fast_lock_manager != nil && !resizing_fast_lock_manager.conflict_maped {
            self.glock.Unlock()
            return resizing_fast_lock_manager
        }
    }

    lock_manager, ok := self.locks[command.LockKey]
    if ok {
        self.glock.Unlock()
        return lock_manager
    }

    if self.free_lock_manager_count >= 0{
        lock_manager = self.free_lock_managers[self.free_lock_manager_count]
        self.free_lock_manager_count--
        lock_manager.freed = false
        if fast_lock_manager == nil {
            self.fast_locks[fast_lock_base_index][fast_lock_index] = lock_manager
        } else {
            if !fast_lock_manager.conflict_maped {
                fast_lock_manager.conflict_maped = true
                self.locks[fast_lock_manager.lock_key] = fast_lock_manager
            }
            fast_lock_manager.ref_count++
            self.locks[command.LockKey] = lock_manager
        }
        self.state.KeyCount++
        self.glock.Unlock()

        lock_manager.lock_key = command.LockKey
    }else{
        lock_managers := make([]LockManager, 4096)

        for i := 0; i < 4096; i++ {
            lock_managers[i].lock_db = self
            lock_managers[i].db_id = command.DbId
            lock_managers[i].locks = NewLockQueue(4, 16, 4)
            lock_managers[i].lock_maps = make(map[[2]uint64]*Lock, 8)
            lock_managers[i].wait_locks = NewLockQueue(4, 16, 4)
            lock_managers[i].glock = self.manager_glocks[self.manager_glock_index]
            lock_managers[i].glock_index = self.manager_glock_index
            lock_managers[i].free_locks = self.free_locks[self.manager_glock_index]
            lock_managers[i].conflict_maped = false

            self.manager_glock_index++
            if self.manager_glock_index >= self.manager_max_glocks {
                self.manager_glock_index = 0
            }
            self.free_lock_manager_count++
            self.free_lock_managers[self.free_lock_manager_count] = &lock_managers[i]
        }

        lock_manager = self.free_lock_managers[self.free_lock_manager_count]
        self.free_lock_manager_count--
        lock_manager.freed = false
        if fast_lock_manager == nil {
            self.fast_locks[fast_lock_base_index][fast_lock_index] = lock_manager
        } else {
            if !fast_lock_manager.conflict_maped {
                fast_lock_manager.conflict_maped = true
                self.locks[fast_lock_manager.lock_key] = fast_lock_manager
            }
            fast_lock_manager.ref_count++
            self.locks[command.LockKey] = lock_manager
        }
        self.state.KeyCount++
        self.glock.Unlock()

        lock_manager.lock_key = command.LockKey
    }
    return lock_manager
}

func (self *LockDB) GetLockManager(command *protocol.LockCommand) *LockManager{
    self.glock.Lock()

    fast_lock_manager := self.fast_locks[(command.LockKey[1] & self.fast_lock_count) >> 23][command.LockKey[1] & FAST_LOCK_SEG_LENGTH_MASK]
    if fast_lock_manager != nil {
        if !fast_lock_manager.conflict_maped {
            self.glock.Unlock()
            return fast_lock_manager
        }
    } else if self.resizing_fast_lock_count != 0 {
        resizing_fast_lock_manager := self.fast_locks[(command.LockKey[1] & self.resizing_fast_lock_count) >> 23][command.LockKey[1] & FAST_LOCK_SEG_LENGTH_MASK]
        if resizing_fast_lock_manager != nil && !resizing_fast_lock_manager.conflict_maped {
            self.glock.Unlock()
            return resizing_fast_lock_manager
        }
    }

    lock_manager, ok := self.locks[command.LockKey]
    if ok {
        self.glock.Unlock()
        return lock_manager
    }

    self.glock.Unlock()
    return nil
}

func (self *LockDB) RemoveFastLockManager(lock_manager *LockManager, fast_lock_manager *LockManager, fast_lock_base_index uint64, fast_lock_index uint64){
    delete(self.locks, lock_manager.lock_key)
    if lock_manager == fast_lock_manager {
        fast_lock_manager.conflict_maped = false
        self.fast_locks[fast_lock_base_index][fast_lock_index] = nil
    } else {
        fast_lock_manager.ref_count--
        if fast_lock_manager.ref_count == 0 {
            fast_lock_manager.conflict_maped = false
            self.fast_locks[fast_lock_base_index][fast_lock_index] = nil
            fast_lock_manager.freed = true

            if self.free_lock_manager_count < 4194303 {
                self.free_lock_manager_count++
                self.free_lock_managers[self.free_lock_manager_count] = fast_lock_manager
                self.state.KeyCount--

                if fast_lock_manager.locks != nil {
                    fast_lock_manager.locks.Reset()
                }
                if fast_lock_manager.wait_locks != nil {
                    fast_lock_manager.wait_locks.Reset()
                }
            } else {
                self.state.KeyCount--

                fast_lock_manager.current_lock = nil
                fast_lock_manager.locks = nil
                fast_lock_manager.lock_maps = nil
                fast_lock_manager.wait_locks = nil
                fast_lock_manager.free_locks = nil
            }
        }
    }
}

func (self *LockDB) RemoveLockManager(lock_manager *LockManager){
    self.glock.Lock()
    if !lock_manager.freed {
        fast_lock_base_index := (lock_manager.lock_key[1] & self.fast_lock_count) >> 23
        fast_lock_index := lock_manager.lock_key[1] & FAST_LOCK_SEG_LENGTH_MASK

        fast_lock_manager := self.fast_locks[fast_lock_base_index][fast_lock_index]
        if fast_lock_manager != nil {
            if !fast_lock_manager.conflict_maped {
                self.fast_locks[fast_lock_base_index][fast_lock_index] = nil
            } else {
                self.RemoveFastLockManager(lock_manager, fast_lock_manager, fast_lock_base_index, fast_lock_index)
            }
        } else if self.resizing_fast_lock_count != 0 {
            fast_lock_base_index = (lock_manager.lock_key[1] & self.resizing_fast_lock_count) >> 23

            fast_lock_manager = self.fast_locks[(lock_manager.lock_key[1] & self.resizing_fast_lock_count) >> 23][lock_manager.lock_key[1] & FAST_LOCK_SEG_LENGTH_MASK]
            if !fast_lock_manager.conflict_maped {
                self.fast_locks[fast_lock_base_index][fast_lock_index] = nil
            } else {
                self.RemoveFastLockManager(lock_manager, fast_lock_manager, fast_lock_base_index, fast_lock_index)
            }
        }
        lock_manager.freed = true

        if self.free_lock_manager_count < 4194303 {
            self.free_lock_manager_count++
            self.free_lock_managers[self.free_lock_manager_count] = lock_manager
            self.state.KeyCount--
            self.glock.Unlock()

            if lock_manager.locks != nil {
                lock_manager.locks.Reset()
            }
            if lock_manager.wait_locks != nil {
                lock_manager.wait_locks.Reset()
            }
        } else {
            self.state.KeyCount--
            self.glock.Unlock()

            lock_manager.current_lock = nil
            lock_manager.locks = nil
            lock_manager.lock_maps = nil
            lock_manager.wait_locks = nil
            lock_manager.free_locks = nil
        }
        return
    }

    self.glock.Unlock()
}

func (self *LockDB) AddTimeOut(lock *Lock){
    lock.timeouted = false

    if lock.timeout_checked_count > 5 {
        timeout_time := self.check_timeout_time + 5
        if lock.timeout_time < timeout_time {
            timeout_time = lock.timeout_time
            if timeout_time < self.check_timeout_time {
                timeout_time = self.check_timeout_time
            }
        }

        self.timeout_locks[timeout_time & TIMEOUT_QUEUE_LENGTH_MASK][lock.manager.glock_index].Push(lock)
    } else {
        timeout_time := self.check_timeout_time + lock.timeout_checked_count
        if lock.timeout_time < timeout_time {
            timeout_time = lock.timeout_time
            if timeout_time < self.check_timeout_time {
                timeout_time = self.check_timeout_time
            }
        }

        self.timeout_locks[timeout_time & TIMEOUT_QUEUE_LENGTH_MASK][lock.manager.glock_index].Push(lock)
    }
}

func (self *LockDB) RemoveTimeOut(lock *Lock){
    lock.timeouted = true
    atomic.AddUint32(&self.state.WaitCount, 0xffffffff)
}

func (self *LockDB) DoTimeOut(lock *Lock){
    lock_manager := lock.manager
    lock_manager.glock.Lock()
    if lock.timeouted {
        lock.ref_count--
        if lock.ref_count == 0 {
            lock_manager.FreeLock(lock)
        }

        if lock_manager.ref_count == 0 {
            self.RemoveLockManager(lock_manager)
        }
        lock_manager.glock.Unlock()
        return
    }

    lock.timeouted = true
    lock_protocol, lock_command := lock.protocol, lock.command
    lock_manager.GetWaitLock()
    lock.ref_count--
    if lock.ref_count == 0 {
        lock_manager.FreeLock(lock)
    }

    if lock_manager.ref_count == 0 {
        self.RemoveLockManager(lock_manager)
    }
    lock_manager.glock.Unlock()

    self.slock.Active(lock_protocol, lock_command, protocol.RESULT_TIMEOUT, lock_manager.locked, false)
    self.slock.FreeLockCommand(lock_command)
    atomic.AddUint32(&self.state.WaitCount, 0xffffffff)
    atomic.AddUint32(&self.state.TimeoutedCount, 1)

    self.slock.Log().Infof("LockTimeout DbId:%d LockKey:%x LockId:%x RequestId:%x RemoteAddr:%s", lock_command.DbId,
        self.ConvertUint642ToByte16(lock_command.LockKey), self.ConvertUint642ToByte16(lock_command.LockId),
        self.ConvertUint642ToByte16(lock_command.RequestId), lock_protocol.RemoteAddr().String())
}

func (self *LockDB) AddExpried(lock *Lock){
    lock.expried = false

    if lock.expried_checked_count > 5 {
        expried_time := self.check_expried_time + 5
        if lock.expried_time < expried_time {
            expried_time = lock.expried_time
            if expried_time < self.check_expried_time {
                expried_time = self.check_expried_time
            }
        }

        self.expried_locks[expried_time & EXPRIED_QUEUE_LENGTH_MASK][lock.manager.glock_index].Push(lock)
    }else{
        expried_time := self.check_expried_time + lock.expried_checked_count
        if lock.expried_time < expried_time {
            expried_time = lock.expried_time
            if expried_time < self.check_expried_time {
                expried_time = self.check_expried_time
            }
        }

        self.expried_locks[expried_time & EXPRIED_QUEUE_LENGTH_MASK][lock.manager.glock_index].Push(lock)
    }
}

func (self *LockDB) RemoveExpried(lock *Lock){
    lock.expried = true
}

func (self *LockDB) DoExpried(lock *Lock){
    lock_manager := lock.manager
    lock_manager.glock.Lock()
    if lock.expried {
        lock.ref_count--
        if lock.ref_count == 0 {
            lock_manager.FreeLock(lock)
        }

        if lock_manager.ref_count == 0 {
            self.RemoveLockManager(lock_manager)
        }
        lock_manager.glock.Unlock()
        return
    }

    lock_locked := lock.locked
    lock.expried = true
    lock_manager.locked-=uint16(lock_locked)
    lock_protocol, lock_command := lock.protocol, lock.command
    lock_manager.RemoveLock(lock)

    wait_lock := lock_manager.GetWaitLock()
    lock.ref_count--
    if lock.ref_count == 0 {
        lock_manager.FreeLock(lock)
    }

    if lock_manager.ref_count == 0 {
        self.RemoveLockManager(lock_manager)
    }
    lock_manager.glock.Unlock()

    self.slock.Active(lock_protocol, lock_command, protocol.RESULT_EXPRIED, lock_manager.locked, false)
    self.slock.FreeLockCommand(lock_command)
    atomic.AddUint32(&self.state.LockedCount, 0xffffffff - uint32(lock_locked) + 1)
    atomic.AddUint32(&self.state.ExpriedCount, uint32(lock_locked))

    self.slock.Log().Infof("LockExpried DbId:%d LockKey:%x LockId:%x RequestId:%x RemoteAddr:%s", lock_command.DbId,
        self.ConvertUint642ToByte16(lock_command.LockKey), self.ConvertUint642ToByte16(lock_command.LockId),
        self.ConvertUint642ToByte16(lock_command.RequestId), lock_protocol.RemoteAddr().String())

    if wait_lock != nil {
        lock_manager.glock.Lock()
        for ;; {
            if !self.DoLock(lock_manager, wait_lock) {
                lock_manager.glock.Unlock()
                return
            }

            wait_lock = self.WakeUpWaitLock(lock_manager, wait_lock, nil)
            if wait_lock !=  nil {
                lock_manager.glock.Lock()
                continue
            }
            lock_manager.waited = false
            return
        }

    }
}

func (self *LockDB) Lock(server_protocol *ServerProtocol, command *protocol.LockCommand) (err error) {
    lock_manager := self.GetOrNewLockManager(command)
    lock_manager.glock.Lock()

    if lock_manager.freed {
        lock_manager.glock.Unlock()
        return self.Lock(server_protocol, command)
    }

    if lock_manager.locked > 0 {
        if command.Flag == 0x01 {
            lock_manager.glock.Unlock()

            current_lock := lock_manager.current_lock
            command.LockId = current_lock.command.LockId
            command.Expried = uint32(current_lock.expried_time - current_lock.start_time)
            command.Timeout = current_lock.command.Timeout
            command.Count = current_lock.command.Count
            command.Rcount = current_lock.command.Rcount

            self.slock.Active(server_protocol, command, protocol.RESULT_UNOWN_ERROR, lock_manager.locked, true)
            server_protocol.FreeLockCommand(command)
            return nil
        }

        current_lock := lock_manager.GetLockedLock(command)
        if current_lock != nil {
            if command.Flag == 0x02 {
                lock_manager.UpdateLockedLock(current_lock, command.Timeout, command.Expried, command.Count, command.Rcount)
                lock_manager.glock.Unlock()

                command.Expried = uint32(current_lock.expried_time - current_lock.start_time)
                command.Timeout = current_lock.command.Timeout
                command.Count = current_lock.command.Count
                command.Rcount = current_lock.command.Rcount
            } else if(current_lock.locked <= command.Rcount){
                if(command.Expried == 0) {
                    lock_manager.glock.Unlock()

                    command.Expried = uint32(current_lock.expried_time - current_lock.start_time)
                    command.Timeout = current_lock.command.Timeout
                    command.Count = current_lock.command.Count
                    command.Rcount = current_lock.command.Rcount

                    self.slock.Active(server_protocol, command, protocol.RESULT_LOCKED_ERROR, uint16(current_lock.locked), true)
                    server_protocol.FreeLockCommand(command)
                    return nil
                }

                lock_manager.locked++
                current_lock.locked++
                lock_manager.UpdateLockedLock(current_lock, command.Timeout, command.Expried, command.Count, command.Rcount)
                lock_manager.glock.Unlock()

                self.slock.Active(server_protocol, command, protocol.RESULT_SUCCED, lock_manager.locked, true)
                server_protocol.FreeLockCommand(command)
                atomic.AddUint64(&self.state.LockCount, 1)
                atomic.AddUint32(&self.state.LockedCount, 1)
                return nil
            } else {
                lock_manager.glock.Unlock()
            }

            self.slock.Active(server_protocol, command, protocol.RESULT_LOCKED_ERROR, lock_manager.locked, true)
            server_protocol.FreeLockCommand(command)
            return nil
        }
    }

    lock := lock_manager.GetOrNewLock(server_protocol, command)
    if self.DoLock(lock_manager, lock) {
        if command.Expried > 0 {
            lock_manager.AddLock(lock)
            lock_manager.locked++
            self.AddExpried(lock)
            lock.ref_count++
            lock_manager.glock.Unlock()

            self.slock.Active(server_protocol, command, protocol.RESULT_SUCCED, lock_manager.locked, true)
            atomic.AddUint64(&self.state.LockCount, 1)
            atomic.AddUint32(&self.state.LockedCount, 1)
            return nil
        }

        lock_manager.FreeLock(lock)
        if lock_manager.ref_count == 0 {
            self.RemoveLockManager(lock_manager)
        }
        lock_manager.glock.Unlock()

        self.slock.Active(server_protocol, command, protocol.RESULT_SUCCED, lock_manager.locked, true)
        server_protocol.FreeLockCommand(command)
        atomic.AddUint64(&self.state.LockCount, 1)
        return nil
    }

    if command.Timeout > 0 {
        lock_manager.AddWaitLock(lock)
        self.AddTimeOut(lock)
        lock.ref_count++
        lock_manager.glock.Unlock()

        atomic.AddUint32(&self.state.WaitCount, 1)
        return nil
    }

    lock_manager.FreeLock(lock)
    if lock_manager.ref_count == 0 {
        self.RemoveLockManager(lock_manager)
    }
    lock_manager.glock.Unlock()

    self.slock.Active(server_protocol, command, protocol.RESULT_TIMEOUT, lock_manager.locked, true)
    server_protocol.FreeLockCommand(command)
    return nil
}

func (self *LockDB) UnLock(server_protocol *ServerProtocol, command *protocol.LockCommand) (err error) {
    lock_manager := self.GetLockManager(command)
    if lock_manager == nil {
        self.slock.Active(server_protocol, command, protocol.RESULT_UNLOCK_ERROR, 0, true)
        server_protocol.FreeLockCommand(command)
        atomic.AddUint32(&self.state.UnlockErrorCount, 1)
        return nil
    }

    lock_manager.glock.Lock()

    if lock_manager.locked == 0 {
        lock_manager.glock.Unlock()

        self.slock.Active(server_protocol, command, protocol.RESULT_UNLOCK_ERROR, lock_manager.locked, true)
        server_protocol.FreeLockCommand(command)
        atomic.AddUint32(&self.state.UnlockErrorCount, 1)
        return nil
    }

    current_lock := lock_manager.GetLockedLock(command)
    if current_lock == nil {
        current_lock = lock_manager.current_lock

        if command.Flag == 0x01 {
            if current_lock == nil {
                lock_manager.glock.Unlock()

                self.slock.Active(server_protocol, command, protocol.RESULT_UNOWN_ERROR, lock_manager.locked, true)
                server_protocol.FreeLockCommand(command)
                atomic.AddUint32(&self.state.UnlockErrorCount, 1)
                return nil
            }

            command.LockId = current_lock.command.LockId
        } else {
            lock_manager.glock.Unlock()

            self.slock.Active(server_protocol, command, protocol.RESULT_UNOWN_ERROR, lock_manager.locked, true)
            server_protocol.FreeLockCommand(command)
            atomic.AddUint32(&self.state.UnlockErrorCount, 1)
            return nil
        }
    }

    wait_lock := lock_manager.GetWaitLock()
    if current_lock.locked > 1 {
        if command.Rcount == 0 {
            //self.RemoveExpried(current_lock)
            lock_locked := current_lock.locked
            current_lock.expried = true
            current_lock_command := current_lock.command
            lock_manager.RemoveLock(current_lock)
            lock_manager.locked-=uint16(lock_locked)
            lock_manager.glock.Unlock()

            self.slock.Active(server_protocol, command, protocol.RESULT_SUCCED, lock_manager.locked, true)
            server_protocol.FreeLockCommand(command)
            server_protocol.FreeLockCommand(current_lock_command)

            atomic.AddUint64(&self.state.UnLockCount, uint64(lock_locked))
            atomic.AddUint32(&self.state.LockedCount, 0xffffffff - uint32(lock_locked) + 1)
        } else {
            lock_manager.locked--
            current_lock.locked--
            lock_manager.glock.Unlock()

            self.slock.Active(server_protocol, command, protocol.RESULT_SUCCED, lock_manager.locked, true)
            server_protocol.FreeLockCommand(command)

            atomic.AddUint64(&self.state.UnLockCount, 1)
            atomic.AddUint32(&self.state.LockedCount, 0xffffffff)
        }
    } else {
        //self.RemoveExpried(current_lock)
        current_lock.expried = true
        current_lock_command := current_lock.command
        lock_manager.RemoveLock(current_lock)
        lock_manager.locked--
        lock_manager.glock.Unlock()

        self.slock.Active(server_protocol, command, protocol.RESULT_SUCCED, lock_manager.locked, true)
        server_protocol.FreeLockCommand(command)
        server_protocol.FreeLockCommand(current_lock_command)

        atomic.AddUint64(&self.state.UnLockCount, 1)
        atomic.AddUint32(&self.state.LockedCount, 0xffffffff)
    }

    if wait_lock != nil {
        lock_manager.glock.Lock()
        for ;; {
            if !self.DoLock(lock_manager, wait_lock) {
                lock_manager.glock.Unlock()
                return nil
            }

            wait_lock = self.WakeUpWaitLock(lock_manager, wait_lock, server_protocol)
            if wait_lock !=  nil {
                lock_manager.glock.Lock()
                continue
            }
            lock_manager.waited = false
            return nil
        }

    }
    return nil
}

func (self *LockDB) DoLock(lock_manager *LockManager, lock *Lock) bool{
    if lock_manager.locked == 0 {
        return true
    }

    if lock_manager.waited {
        return false
    }

    if(lock_manager.locked <= lock_manager.current_lock.command.Count){
        if(lock_manager.locked <= lock.command.Count) {
            return true
        }
    }

    return false
}

func (self *LockDB) WakeUpWaitLock(lock_manager *LockManager, wait_lock *Lock, server_protocol *ServerProtocol) *Lock {
    if wait_lock.timeouted {
        wait_lock = lock_manager.GetWaitLock()
        lock_manager.glock.Unlock()
        return wait_lock
    }

    //self.RemoveTimeOut(wait_lock)
    wait_lock.timeouted = true

    if wait_lock.command.Expried > 0 {
        lock_manager.AddLock(wait_lock)
        lock_manager.locked++
        self.AddExpried(wait_lock)
        wait_lock.ref_count++
        lock_manager.GetWaitLock()
        lock_manager.glock.Unlock()

        self.slock.Active(wait_lock.protocol, wait_lock.command, protocol.RESULT_SUCCED, lock_manager.locked, wait_lock.protocol == server_protocol)
        atomic.AddUint64(&self.state.LockCount, 1)
        atomic.AddUint32(&self.state.LockedCount, 1)
        atomic.AddUint32(&self.state.WaitCount, 0xffffffff)
        return nil
    }

    wait_lock_protocol, wait_lock_command := wait_lock.protocol, wait_lock.command
    wait_lock = lock_manager.GetWaitLock()
    if lock_manager.ref_count == 0 {
        self.RemoveLockManager(lock_manager)
    }
    lock_manager.glock.Unlock()

    if wait_lock_protocol == server_protocol {
        self.slock.Active(wait_lock_protocol, wait_lock_command, protocol.RESULT_SUCCED, lock_manager.locked, true)
        server_protocol.FreeLockCommand(wait_lock_command)
    } else {
        self.slock.Active(wait_lock_protocol, wait_lock_command, protocol.RESULT_SUCCED, lock_manager.locked, false)
        self.slock.FreeLockCommand(wait_lock_command)
    }

    atomic.AddUint64(&self.state.LockCount, 1)
    atomic.AddUint32(&self.state.WaitCount, 0xffffffff)
    return wait_lock
}

func (self *LockDB) GetState() *protocol.LockDBState {
    return &self.state
}
