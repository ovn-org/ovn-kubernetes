package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	observ "github.com/ovn-org/ovn-kubernetes/go-controller/observability-lib"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)
	go func() {
		<-sigc
		fmt.Println("Received a signal, terminating.")
		cancel()
	}()
	enableDecoder := flag.Bool("enable-enrichment", true, "Enrich samples with nbdb data.")
	logCookie := flag.Bool("log-cookie", false, "Print raw sample cookie with psample group_id.")
	printPacket := flag.Bool("print-full-packet", false, "Print full received packet. When false, only src and dst ips are printed with every sample.")
	addOVSCollector := flag.Bool("add-ovs-collector", false, "Add ovs collector to enable sampling. Use with caution. Make sure no one else is using observability.")
	outputFile := flag.String("output-file", "", "Output file to write the samples to.")
	filterSrcIP := flag.String("filter-src-ip", "", "Filter in only packets from a given source ip.")
	filterDstIP := flag.String("filter-dst-ip", "", "Filter in only packets to a given destination ip.")
	flag.Parse()

	reader := observ.NewSampleReader(*enableDecoder, *logCookie, *printPacket, *addOVSCollector, *filterSrcIP, *filterDstIP, *outputFile)
	err := reader.ReadSamples(ctx)
	if err != nil {
		fmt.Println(err.Error())
	}
}
