package control

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"strings"

	servicecontext "github.com/pydio/cells/common/service/context"

	"github.com/pydio/cells/common/log"

	"github.com/pydio/cells-sync/common"
)

type SpawnedService struct {
	name   string
	args   []string
	cancel context.CancelFunc
	logCtx context.Context
}

func NewSpawnedService(name string, args []string) *SpawnedService {
	s := &SpawnedService{
		name: name,
		args: args,
	}
	ctx := servicecontext.WithServiceName(context.Background(), name)
	ctx = servicecontext.WithServiceColor(ctx, servicecontext.ServiceColorOther)
	s.logCtx = ctx
	return s
}

func (c *SpawnedService) Serve() {
	var ctx context.Context
	log.Logger(c.logCtx).Info("Starting sub-process")
	ctx, c.cancel = context.WithCancel(c.logCtx)
	cmd := exec.CommandContext(ctx, common.ProcessName(os.Args[0]), c.args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return // PRINT SOMETHING
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return // PRINT SOMETHING
	}
	scannerOut := bufio.NewScanner(stdout)
	go func() {
		for scannerOut.Scan() {
			text := strings.TrimRight(scannerOut.Text(), "\n")
			log.Logger(c.logCtx).Info(text)
		}
	}()
	scannerErr := bufio.NewScanner(stderr)
	go func() {
		for scannerErr.Scan() {
			text := strings.TrimRight(scannerErr.Text(), "\n")
			log.Logger(c.logCtx).Error(text)
		}
	}()
	if e := cmd.Run(); e != nil {
		log.Logger(c.logCtx).Error("Error on sub process : " + e.Error())
		c.cancel = nil
		panic(e)
	}
}

func (c *SpawnedService) Stop() {
	log.Logger(c.logCtx).Info("Stopping sub process")
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
}