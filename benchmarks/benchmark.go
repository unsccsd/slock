package main

import (
    "slock"
    "fmt"
    "time"
)

type Count struct {
    count int64
}

func run(client *slock.Client, count *Count) {
    for ;; {
        lock_key := client.SelectDB(0).GenLockId()
        lock := client.Lock(lock_key, 5, 5)
        lock.Lock()
        lock.Unlock()
        count.count++
        if count.count > 1000000 {
            return
        }
    }
}

func main()  {
    clients := make([]*slock.Client, 201)

    for c:=0;c<201;c++ {
        client := slock.NewClient("127.0.0.1", 5658)
        err := client.Open()
        if err != nil {
            fmt.Printf("Connect Error: %v", err)
            return
        }
        clients[c] = client
    }

    count := &Count{}
    start_time := time.Now().Unix()
    for i:=0;i<200;i++{
        go run(clients[i], count)
    }
    run(clients[200], count)
    end_time := time.Now().Unix()
    pt := end_time - start_time
    fmt.Printf("%d %d", pt, count.count / pt)
}
