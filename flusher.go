package veneur

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
)

// Flush takes the slices of metrics, combines then and marshals them to json
// for posting to Datadog.
func (s *Server) Flush(interval time.Duration, metricLimit int) {
	percentiles := s.HistogramPercentiles
	if s.ForwardAddr != "" {
		// don't publish percentiles if we're a local veneur; that's the global
		// veneur's job
		percentiles = nil
	}

	// allocating this long array to count up the sizes is cheaper than appending
	// the []DDMetrics together one at a time
	tempMetrics := make([]WorkerMetrics, 0, len(s.Workers))
	var (
		totalCounters   int
		totalGauges     int
		totalHistograms int
		totalSets       int
		totalTimers     int
	)
	for i, w := range s.Workers {
		s.logger.WithField("worker", i).Debug("Flushing")
		wm := w.Flush()
		tempMetrics = append(tempMetrics, wm)

		totalCounters += len(wm.counters)
		totalGauges += len(wm.gauges)
		totalHistograms += len(wm.histograms)
		totalSets += len(wm.sets)
		totalTimers += len(wm.timers)
	}

	totalLength := totalCounters + totalGauges + (totalTimers+totalHistograms)*(HistogramLocalLength+len(percentiles))
	if s.ForwardAddr == "" {
		totalLength += totalSets
	}
	finalMetrics := make([]DDMetric, 0, totalLength)
	for _, wm := range tempMetrics {
		for _, c := range wm.counters {
			finalMetrics = append(finalMetrics, c.Flush(interval)...)
		}
		for _, g := range wm.gauges {
			finalMetrics = append(finalMetrics, g.Flush()...)
		}
		// if we're a local veneur, then percentiles=nil, and only the local
		// parts (count, min, max) will be flushed
		for _, h := range wm.histograms {
			finalMetrics = append(finalMetrics, h.Flush(interval, percentiles)...)
		}
		for _, t := range wm.timers {
			finalMetrics = append(finalMetrics, t.Flush(interval, percentiles)...)
		}
		if s.ForwardAddr == "" {
			// sets have no local parts, so if we're a local veneur, there's
			// nothing to flush at all
			for _, s := range wm.sets {
				finalMetrics = append(finalMetrics, s.Flush()...)
			}
		}
	}
	for i := range finalMetrics {
		finalMetrics[i].Hostname = s.Hostname
		finalMetrics[i].Tags = append(finalMetrics[i].Tags, s.Tags...)
	}

	s.statsd.Count("worker.metrics_flushed_total", int64(totalCounters), []string{"metric_type:counter"}, 1.0)
	s.statsd.Count("worker.metrics_flushed_total", int64(totalGauges), []string{"metric_type:gauge"}, 1.0)
	if s.ForwardAddr == "" {
		// only report these lengths if we're the global veneur instance
		// responsible for flushing them
		// this avoids double-counting problems where a local veneur reports
		// histograms that it received, and then a global veneur reports them
		// again
		s.statsd.Count("worker.metrics_flushed_total", int64(totalHistograms), []string{"metric_type:histogram"}, 1.0)
		s.statsd.Count("worker.metrics_flushed_total", int64(totalSets), []string{"metric_type:set"}, 1.0)
		s.statsd.Count("worker.metrics_flushed_total", int64(totalTimers), []string{"metric_type:timer"}, 1.0)
	}

	if s.ForwardAddr != "" {
		// we cannot do this until we're done using tempMetrics here, since
		// not everything in tempMetrics is safe for sharing
		go s.flushForward(tempMetrics)
	}

	s.statsd.Gauge("flush.post_metrics_total", float64(len(finalMetrics)), nil, 1.0)
	// Check to see if we have anything to do
	if len(finalMetrics) == 0 {
		s.logger.Info("Nothing to flush, skipping.")
		return
	}

	// break the metrics into chunks of approximately equal size, such that
	// each chunk is less than the limit
	// we compute the chunks using rounding-up integer division
	workers := ((len(finalMetrics) - 1) / metricLimit) + 1
	chunkSize := ((len(finalMetrics) - 1) / workers) + 1
	s.logger.WithField("workers", workers).Debug("Worker count chosen")
	s.logger.WithField("chunkSize", chunkSize).Debug("Chunk size chosen")
	var wg sync.WaitGroup
	flushStart := time.Now()
	for i := 0; i < workers; i++ {
		chunk := finalMetrics[i*chunkSize:]
		if i < workers-1 {
			// trim to chunk size unless this is the last one
			chunk = chunk[:chunkSize]
		}
		wg.Add(1)
		go s.flushPart(chunk, &wg)
	}
	wg.Wait()
	s.statsd.TimeInMilliseconds("flush.total_duration_ns", float64(time.Now().Sub(flushStart).Nanoseconds()), nil, 1.0)

	s.logger.WithField("metrics", len(finalMetrics)).Info("Completed flush to Datadog")
}

func (s *Server) flushPart(metricSlice []DDMetric, wg *sync.WaitGroup) {
	defer wg.Done()
	s.postHelper(fmt.Sprintf("%s/api/v1/series?api_key=%s", s.DDHostname, s.DDAPIKey), map[string][]DDMetric{
		"series": metricSlice,
	}, "flush")
}

func (s *Server) flushForward(wms []WorkerMetrics) {
	jmLength := 0
	for _, wm := range wms {
		jmLength += len(wm.histograms)
		jmLength += len(wm.sets)
		jmLength += len(wm.timers)
	}

	jsonMetrics := make([]JSONMetric, 0, jmLength)
	for _, wm := range wms {
		for _, histo := range wm.histograms {
			jm, err := histo.Export()
			if err != nil {
				s.logger.WithFields(logrus.Fields{
					logrus.ErrorKey: err,
					"type":          "histogram",
					"name":          histo.name,
				}).Error("Could not export metric")
				continue
			}
			jsonMetrics = append(jsonMetrics, jm)
		}
		for _, set := range wm.sets {
			jm, err := set.Export()
			if err != nil {
				s.logger.WithFields(logrus.Fields{
					logrus.ErrorKey: err,
					"type":          "set",
					"name":          set.name,
				}).Error("Could not export metric")
				continue
			}
			jsonMetrics = append(jsonMetrics, jm)
		}
		for _, timer := range wm.timers {
			jm, err := timer.Export()
			if err != nil {
				s.logger.WithFields(logrus.Fields{
					logrus.ErrorKey: err,
					"type":          "timer",
					"name":          timer.name,
				}).Error("Could not export metric")
				continue
			}
			jsonMetrics = append(jsonMetrics, jm)
		}
	}

	s.statsd.Gauge("forward.post_metrics_total", float64(len(jsonMetrics)), nil, 1.0)
	if len(jsonMetrics) == 0 {
		s.logger.Info("Nothing to forward, skipping.")
		return
	}

	// always re-resolve the host to avoid dns caching
	endpoint, err := resolveEndpoint(fmt.Sprintf("%s/import", s.ForwardAddr))
	if err != nil {
		// not a fatal error if we fail
		// we'll just try to use the host as it was given to us
		s.statsd.Count("forward.error_total", 1, []string{"cause:dns"}, 1.0)
		s.logger.WithError(err).Warn("Could not re-resolve host for forward")
	}

	// the error has already been logged (if there was one), so we only care
	// about the success case
	if s.postHelper(endpoint, jsonMetrics, "forward") == nil {
		s.logger.WithField("metrics", len(jsonMetrics)).Info("Completed forward to upstream Veneur")
	}
}

// given a url, attempts to resolve the url's host, and returns a new url whose
// host has been replaced by the first resolved address
// on failure, it returns the argument, and the resulting error
func resolveEndpoint(endpoint string) (string, error) {
	origURL, err := url.Parse(endpoint)
	if err != nil {
		// caution: this error contains the endpoint itself, so if the endpoint
		// has secrets in it, you have to remove them
		return endpoint, err
	}

	origHost, origPort, err := net.SplitHostPort(origURL.Host)
	if err != nil {
		return endpoint, err
	}

	resolvedNames, err := net.LookupHost(origHost)
	if err != nil {
		return endpoint, err
	}
	if len(resolvedNames) == 0 {
		return endpoint, &net.DNSError{
			Err:  "no hosts found",
			Name: origHost,
		}
	}

	origURL.Host = net.JoinHostPort(resolvedNames[0], origPort)
	return origURL.String(), nil
}

// shared code for POSTing to an endpoint, that consumes JSON, that is zlib-
// compressed, that returns 202 on success, that has a small response
// action is a string used for statsd metric names and log messages emitted from
// this function - probably a static string for each callsite
func (s *Server) postHelper(endpoint string, bodyObject interface{}, action string) error {
	// attach this field to all the logs we generate
	innerLogger := s.logger.WithField("action", action)

	marshalStart := time.Now()
	var bodyBuffer bytes.Buffer
	compressor := zlib.NewWriter(&bodyBuffer)
	encoder := json.NewEncoder(compressor)
	if err := encoder.Encode(bodyObject); err != nil {
		s.statsd.Count(action+".error_total", 1, []string{"cause:json"}, 1.0)
		innerLogger.WithError(err).Error("Could not render JSON")
		return err
	}
	// don't forget to flush leftover compressed bytes to the buffer
	if err := compressor.Close(); err != nil {
		s.statsd.Count(action+".error_total", 1, []string{"cause:compress"}, 1.0)
		innerLogger.WithError(err).Error("Could not finalize compression")
		return err
	}
	s.statsd.TimeInMilliseconds(action+".duration_ns", float64(time.Now().Sub(marshalStart).Nanoseconds()), []string{"part:json"}, 1.0)

	// Len reports the unread length, so we have to record this before the
	// http client consumes it
	bodyLength := bodyBuffer.Len()
	s.statsd.Histogram(action+".content_length_bytes", float64(bodyLength), nil, 1.0)

	req, err := http.NewRequest(http.MethodPost, endpoint, &bodyBuffer)
	if err != nil {
		s.statsd.Count(action+".error_total", 1, []string{"cause:construct"}, 1.0)
		innerLogger.WithError(err).Error("Could not construct request")
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "deflate")

	requestStart := time.Now()
	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		if urlErr, ok := err.(*url.Error); ok {
			// if the error has the url in it, then retrieve the inner error
			// and ditch the url (which might contain secrets)
			err = urlErr.Err
		}
		s.statsd.Count(action+".error_total", 1, []string{"cause:io"}, 1.0)
		innerLogger.WithError(err).Error("Could not execute request")
		return err
	}
	s.statsd.TimeInMilliseconds(action+".duration_ns", float64(time.Now().Sub(requestStart).Nanoseconds()), []string{"part:post"}, 1.0)
	defer resp.Body.Close()

	responseBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		// this error is not fatal, since we only need the body for reporting
		// purposes
		s.statsd.Count(action+".error_total", 1, []string{"cause:readresponse"}, 1.0)
		innerLogger.WithError(err).Error("Could not read response body")
	}
	resultLogger := innerLogger.WithFields(logrus.Fields{
		"request_length":   bodyLength,
		"request_headers":  req.Header,
		"status":           resp.Status,
		"response_headers": resp.Header,
		"response":         string(responseBody),
	})

	if resp.StatusCode != http.StatusAccepted {
		s.statsd.Count(action+".error_total", 1, []string{fmt.Sprintf("cause:%d", resp.StatusCode)}, 1.0)
		resultLogger.Error("Could not POST")
		return err
	}

	// make sure the error metric isn't sparse
	s.statsd.Count(action+".error_total", 0, nil, 1.0)
	resultLogger.Debug("POSTed successfully")
	return nil
}
