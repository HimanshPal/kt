package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/user"
	"regexp"
	"strings"
	"sync"

	"github.com/Shopify/sarama"
)

type topicArgs struct {
	brokers    string
	filter     string
	partitions bool
	leaders    bool
	replicas   bool
	verbose    bool
	version    string
}

type topicCmd struct {
	brokers    []string
	filter     *regexp.Regexp
	partitions bool
	leaders    bool
	replicas   bool
	verbose    bool
	version    sarama.KafkaVersion

	client sarama.Client
}

type topic struct {
	Name       string      `json:"name"`
	Partitions []partition `json:"partitions,omitempty"`
}

type partition struct {
	Id           int32   `json:"id"`
	OldestOffset int64   `json:"oldest"`
	NewestOffset int64   `json:"newest"`
	Leader       string  `json:"leader,omitempty"`
	Replicas     []int32 `json:"replicas,omitempty"`
}

func (cmd *topicCmd) parseFlags(as []string) topicArgs {
	var (
		args  topicArgs
		flags = flag.NewFlagSet("topic", flag.ExitOnError)
	)

	flags.StringVar(&args.brokers, "brokers", "", "Comma separated list of brokers. Port defaults to 9092 when omitted.")
	flags.BoolVar(&args.partitions, "partitions", false, "Include information per partition.")
	flags.BoolVar(&args.leaders, "leaders", false, "Include leader information per partition.")
	flags.BoolVar(&args.replicas, "replicas", false, "Include replica ids per partition.")
	flags.StringVar(&args.filter, "filter", "", "Regex to filter topics by name.")
	flags.BoolVar(&args.verbose, "verbose", false, "More verbose logging to stderr.")
	flags.StringVar(&args.version, "version", "", "Kafka protocol version")
	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage of topic:")
		flags.PrintDefaults()
		fmt.Fprintln(os.Stderr, `
The values for -brokers can also be set via the environment variable KT_BROKERS respectively.
The values supplied on the command line win over environment variable values.
`)
		os.Exit(2)
	}

	flags.Parse(as)
	return args
}

func (cmd *topicCmd) parseArgs(as []string) {
	var (
		err error
		re  *regexp.Regexp

		args       = cmd.parseFlags(as)
		envBrokers = os.Getenv("KT_BROKERS")
	)
	if args.brokers == "" {
		if envBrokers != "" {
			args.brokers = envBrokers
		} else {
			args.brokers = "localhost:9092"
		}
	}
	cmd.brokers = strings.Split(args.brokers, ",")
	for i, b := range cmd.brokers {
		if !strings.Contains(b, ":") {
			cmd.brokers[i] = b + ":9092"
		}
	}

	if re, err = regexp.Compile(args.filter); err != nil {
		failf("invalid regex for filter err=%s", err)
	}

	cmd.filter = re
	cmd.partitions = args.partitions
	cmd.leaders = args.leaders
	cmd.replicas = args.replicas
	cmd.verbose = args.verbose
	cmd.version = kafkaVersion(args.version)
}

func (cmd *topicCmd) connect() {
	var (
		err error
		usr *user.User
		cfg = sarama.NewConfig()
	)

	cfg.Version = cmd.version

	if usr, err = user.Current(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read current user err=%v", err)
	}
	cfg.ClientID = "kt-topic-" + usr.Username
	if cmd.verbose {
		fmt.Fprintf(os.Stderr, "sarama client configuration %#v\n", cfg)
	}

	if cmd.client, err = sarama.NewClient(cmd.brokers, cfg); err != nil {
		failf("failed to create client err=%v", err)
	}
}

func (cmd *topicCmd) run(as []string, q chan struct{}) {
	var (
		err error
		all []string
		out = make(chan printContext)
	)

	go func() { <-q; failf("received quit signal.") }()

	cmd.parseArgs(as)
	if cmd.verbose {
		sarama.Logger = log.New(os.Stderr, "", log.LstdFlags)
	}

	cmd.connect()
	defer cmd.client.Close()

	if all, err = cmd.client.Topics(); err != nil {
		failf("failed to read topics err=%v", err)
	}

	topics := []string{}
	for _, a := range all {
		if cmd.filter.MatchString(a) {
			topics = append(topics, a)
		}
	}

	go print(out)

	var wg sync.WaitGroup
	for _, tn := range topics {
		wg.Add(1)
		go func(top string) {
			cmd.print(top, out)
			wg.Done()
		}(tn)
	}
	wg.Wait()
}

func (cmd *topicCmd) print(name string, out chan printContext) {
	var (
		top topic
		buf []byte
		err error
	)

	if top, err = cmd.readTopic(name); err != nil {
		fmt.Fprintf(os.Stderr, "failed to read info for topic %s. err=%v\n", name, err)
		return
	}

	if buf, err = json.Marshal(top); err != nil {
		fmt.Fprintf(os.Stderr, "failed to marshal JSON for topic %s. err=%v\n", name, err)
		return
	}

	ctx := printContext{string(buf), make(chan struct{})}
	out <- ctx
	<-ctx.done
}

func (cmd *topicCmd) readTopic(name string) (topic, error) {
	var (
		err error
		ps  []int32
		led *sarama.Broker
		top = topic{Name: name}
	)

	if !cmd.partitions {
		return top, nil
	}

	if ps, err = cmd.client.Partitions(name); err != nil {
		return top, err
	}

	for _, p := range ps {
		np := partition{Id: p}

		if np.OldestOffset, err = cmd.client.GetOffset(name, p, sarama.OffsetOldest); err != nil {
			return top, err
		}

		if np.NewestOffset, err = cmd.client.GetOffset(name, p, sarama.OffsetNewest); err != nil {
			return top, err
		}

		if cmd.leaders {
			if led, err = cmd.client.Leader(name, p); err != nil {
				return top, err
			}
			np.Leader = led.Addr()
		}

		if cmd.replicas {
			if np.Replicas, err = cmd.client.Replicas(name, p); err != nil {
				return top, err
			}
		}

		top.Partitions = append(top.Partitions, np)
	}

	return top, nil
}
