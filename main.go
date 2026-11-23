package main

import (
	"flag"
	"log"
	"net"
	"strconv"
)

func main() {
	ip := flag.String("lip", "127.0.0.1", "gateway listen IP")
	port := flag.Int("lport", 3306, "gateway listen port")
	path := flag.String("config", "config.yaml", "gateway YAML configuration")
	flag.Parse()

	gw, err := loadGateway(*path)
	if err != nil {
		log.Fatal(err)
	}
	l, err := net.Listen("tcp", net.JoinHostPort(*ip, strconv.Itoa(*port)))
	if err != nil {
		log.Fatal(err)
	}
	defer l.Close()
	log.Printf("MySQL gateway listening on %s", l.Addr())
	for {
		c, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go gw.serve(c)
	}
}
