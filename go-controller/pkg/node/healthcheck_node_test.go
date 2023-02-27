package node

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"k8s.io/client-go/tools/record"
)

var _ = Describe("Node healthcheck tests", func() {
	var (
		wg     *sync.WaitGroup
		stopCh chan struct{}
	)

	BeforeEach(func() {
		stopCh = make(chan struct{})
		wg = &sync.WaitGroup{}
	})

	AfterEach(func() {
		close(stopCh)
		wg.Wait()
	})

	Context("node proxy healthz server is started", func() {
		It("it reports healthy", func() {
			recorder := record.NewFakeRecorder(10)
			const addr string = "127.0.0.1:10256"
			hzs := newNodeProxyHealthzServer("some-node", addr, recorder)
			hzs.Start(stopCh, wg)

			// Try a few times to make sure the server is listening,
			// there's a small race between when Start() returns and
			// the ListenAndServe() is actually active
			var err error
			for i := 0; i < 5; i++ {
				resp, err := http.Get(fmt.Sprintf("http://%s/healthz", addr))
				if err == nil {
					defer resp.Body.Close()
					Expect(resp.StatusCode).To(Equal(http.StatusOK))
				}
				time.Sleep(50 * time.Millisecond)
			}
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
