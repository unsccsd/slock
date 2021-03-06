package server

import (
    "github.com/hhkbp2/go-logging"
    "github.com/snower/slock/protocol"
    "sync"
    "time"
)

const (
    STATE_INIT  = iota
    STATE_LEADER
    STATE_FOLLOWER
    STATE_SYNC
)

type SLock struct {
    dbs                         []*LockDB
    glock                       *sync.Mutex
    aof                         *Aof
    admin                       *Admin
    logger                      logging.Logger
    streams                     map[[16]byte]ServerProtocol
    uptime                      *time.Time
    free_lock_commands          *LockCommandQueue
    free_lock_command_lock      *sync.Mutex
    free_lock_command_count     int32
    stats_total_command_count   uint64
    state                       uint8
}

func NewSLock(config *ServerConfig) *SLock {
    SetConfig(config)

    aof := NewAof()
    admin := NewAdmin()
    now := time.Now()
    logger := InitLogger(Config.Log, Config.LogLevel)
    slock := &SLock{make([]*LockDB, 256), &sync.Mutex{}, aof,admin, logger, make(map[[16]byte]ServerProtocol, STREAMS_INIT_COUNT),
        &now,NewLockCommandQueue(16, 64, FREE_COMMAND_QUEUE_INIT_SIZE * 16), &sync.Mutex{}, 0,
        0, STATE_INIT}
    aof.slock = slock
    admin.slock = slock
    return slock
}

func (self *SLock) Init() error {
    err := self.aof.LoadAndInit()
    if err != nil {
        self.logger.Errorf("Aof LoadOrInit Error: %v", err)
        return err
    }
    self.UpdateState(STATE_LEADER)
    return nil
}

func (self *SLock) Close()  {
    defer self.glock.Unlock()
    self.glock.Lock()

    for _, db := range self.dbs {
        if db != nil {
            db.Close()
        }
    }

    self.aof.Close()
    self.admin.Close()
}

func (self *SLock) UpdateState(state uint8)  {
    self.state = state
}

func (self *SLock) GetAof() *Aof {
    return self.aof
}

func (self *SLock) GetAdmin() *Admin {
    return self.admin
}

func (self *SLock) GetOrNewDB(db_id uint8) *LockDB {
    defer self.glock.Unlock()
    self.glock.Lock()

    if self.dbs[db_id] == nil {
        self.dbs[db_id] = NewLockDB(self, db_id)
    }
    return self.dbs[db_id]
}

func (self *SLock) GetDB(db_id uint8) *LockDB {
    if self.dbs[db_id] == nil {
        return self.GetOrNewDB(db_id)
    }
    return self.dbs[db_id]
}

func (self *SLock) DoLockComamnd(db *LockDB, server_protocol ServerProtocol, command *protocol.LockCommand) error {
    return db.Lock(server_protocol, command)
}

func (self *SLock) DoUnLockComamnd(db *LockDB, server_protocol ServerProtocol, command *protocol.LockCommand) error {
    return db.UnLock(server_protocol, command)
}

func (self *SLock) GetState(server_protocol ServerProtocol, command *protocol.StateCommand) error {
    db_state := uint8(0)

    db := self.dbs[command.DbId]
    if db != nil {
        db_state = 1
    }

    if db == nil {
        return server_protocol.Write(protocol.NewStateResultCommand(command, protocol.RESULT_SUCCED, 0, db_state, nil))
    }
    return server_protocol.Write(protocol.NewStateResultCommand(command, protocol.RESULT_SUCCED, 0, db_state, db.GetState()))
}

func (self *SLock) Log() logging.Logger {
    return self.logger
}

func (self *SLock) FreeLockCommand(command *protocol.LockCommand) *protocol.LockCommand{
    self.free_lock_command_lock.Lock()
    if self.free_lock_commands.Push(command) != nil {
        return nil
    }
    self.free_lock_command_count++
    self.free_lock_command_lock.Unlock()
    return command
}

func (self *SLock) GetLockCommand() *protocol.LockCommand{
    self.free_lock_command_lock.Lock()
    command := self.free_lock_commands.PopRight()
    if command != nil {
        self.free_lock_command_count--
    }
    self.free_lock_command_lock.Unlock()
    return command
}

func (self *SLock) FreeLockCommands(commands []*protocol.LockCommand) error{
    self.free_lock_command_lock.Lock()
    for _, command := range commands {
        if self.free_lock_commands.Push(command) != nil {
            continue
        }
        self.free_lock_command_count++
    }
    self.free_lock_command_lock.Unlock()
    return nil
}

func (self *SLock) GetLockCommands(count int32) []*protocol.LockCommand{
    self.free_lock_command_lock.Lock()
    if count > self.free_lock_command_count {
        count = self.free_lock_command_count
    }
    commands := make([]*protocol.LockCommand, count)
    for i := int32(0); i < count; i++ {
        command := self.free_lock_commands.PopRight()
        if command == nil {
            break
        }
        commands[i] = command
        self.free_lock_command_count--
    }
    self.free_lock_command_lock.Unlock()
    return commands
}
