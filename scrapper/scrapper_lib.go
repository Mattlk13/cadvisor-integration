package scrapper_lib

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/signalfx/golib/datapoint"
	"golang.org/x/net/context"
)

type logger interface {
	Printf(msg string, args ...interface{})
}

var _ logger = &log.Logger{}

// Scrapper can fetch prometheus metrics and convert them into datapoints
type Scrapper struct {
	Client    *http.Client
	UserAgent string
	L         logger
}

type cancelableRequest interface {
	CancelRequest(req *http.Request)
}

// Fetch prometheus points from an endpoint and convert them to datapoints
func (s *Scrapper) Fetch(ctx context.Context, endpoint *url.URL, clusterName string) ([]*datapoint.Datapoint, error) {
	req, err := http.NewRequest("GET", endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	accept := fmt.Sprintf("application/vnd.google.protobuf; proto=io.prometheus.client.MetricFamily; encoding=delimited, text/plain; version=%s", prometheus.APIVersion)
	req.Header.Add("Accept", accept)
	if s.UserAgent != "" {
		req.Header.Add("User-Agent", s.UserAgent)
	}
	//s.L.Printf("req: %v\n", req)
	doneWaiting := make(chan struct{})
	defer close(doneWaiting)
	if cr, ok := s.Client.Transport.(cancelableRequest); ok {
		go func() {
			select {
			case <-ctx.Done():
				cr.CancelRequest(req)
			case <-doneWaiting:
			}
		}()
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	// contentType := resp.Header.Get("Content-Type")
	bodyBytes := &bytes.Buffer{}
	_, err = io.Copy(bodyBytes, resp.Body)
	if err != nil {
		return nil, err
	}
	defer func() {
		logIfErr(s.L, resp.Body.Close(), "could not close response body")
	}()
	mf, err := parseAsProto(bodyBytes.Bytes())
	// TODO: Also parse text format
	logIfErr(s.L, err, "Unable to parse protocol buffers. err: %v", err)
	return prometheusToSignalFx(mf, clusterName), err
}

func logIfErr(l logger, err error, msg string, args ...interface{}) {
	if err != nil {
		l.Printf(msg, args...)
	}
}

func prometheusToSignalFx(propoints []*dto.MetricFamily, clusterName string) []*datapoint.Datapoint {
	ret := make([]*datapoint.Datapoint, 0, len(propoints))
	for _, pp := range propoints {
		metricName := pp.GetName()
		for _, m := range pp.Metric {
			tsMs := m.GetTimestampMs()
			dims := make(map[string]string, len(m.GetLabel()))
			for _, l := range m.GetLabel() {
				key := l.GetName()
				value := l.GetValue()
				if key != "" && value != "" {
					dims[key] = value
				}
			}
			dims["clusterName"] = clusterName
			mc := convertMeric(m)
			timestamp := time.Unix(0, tsMs*time.Millisecond.Nanoseconds())
			for _, conv := range mc {
				ret = append(ret, datapoint.New(metricName+conv.metricNameSuffix, appendDims(dims, conv.extraDims), conv.value, conv.mtype, timestamp))
			}
		}
	}
	return ret
}

func appendDims(a map[string]string, b map[string]string) map[string]string {
	ret := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		ret[k] = v
	}
	for k, v := range b {
		ret[k] = v
	}
	return ret
}

func convertMeric(m *dto.Metric) []metricConversion {
	if m.Counter != nil {
		return []metricConversion{
			{
				mtype: datapoint.Counter,
				value: datapoint.NewFloatValue(m.Counter.GetValue()),
			},
		}
	}
	if m.Gauge != nil {
		return []metricConversion{
			{
				mtype: datapoint.Gauge,
				value: datapoint.NewFloatValue(m.Gauge.GetValue()),
			},
		}
	}
	if m.Summary != nil {
		ret := []metricConversion{
			{
				metricNameSuffix: "_sum",
				mtype:            datapoint.Counter,
				value:            datapoint.NewFloatValue(m.Summary.GetSampleSum()),
			},
			{
				metricNameSuffix: "_count",
				mtype:            datapoint.Counter,
				value:            datapoint.NewIntValue(int64(m.Summary.GetSampleCount())),
			},
		}
		for _, q := range m.Summary.Quantile {
			ret = append(ret, metricConversion{
				mtype: datapoint.Gauge,
				value: datapoint.NewIntValue(int64(q.GetValue())),
				extraDims: map[string]string{
					"quantile": fmt.Sprintf("%.2f", q.GetQuantile()*100),
				},
			})
		}
		return ret
	}
	if m.Histogram != nil {
		ret := []metricConversion{
			{
				metricNameSuffix: "_sum",
				mtype:            datapoint.Counter,
				value:            datapoint.NewFloatValue(m.Histogram.GetSampleSum()),
			},
			{
				metricNameSuffix: "_count",
				mtype:            datapoint.Counter,
				value:            datapoint.NewIntValue(int64(m.Histogram.GetSampleCount())),
			},
		}
		for _, b := range m.Histogram.Bucket {
			ret = append(ret, metricConversion{
				mtype:            datapoint.Counter,
				metricNameSuffix: "_bucket",
				value:            datapoint.NewIntValue(int64(b.GetCumulativeCount())),
				extraDims: map[string]string{
					"le": fmt.Sprintf("%.2f", b.GetUpperBound()),
				},
			})
		}
		return ret
	}
	if m.Untyped != nil {
		return []metricConversion{
			{
				mtype: datapoint.Gauge,
				value: datapoint.NewFloatValue(m.Untyped.GetValue()),
			},
		}
	}
	return []metricConversion{}
}

type metricConversion struct {
	mtype            datapoint.MetricType
	value            datapoint.Value
	metricNameSuffix string
	extraDims        map[string]string
}

func parseAsProto(body []byte) ([]*dto.MetricFamily, error) {
	ret := make([]*dto.MetricFamily, 0, len(body)/30+1)
	buf := proto.NewBuffer(body)
	for {
		mf := &dto.MetricFamily{}
		err := buf.DecodeMessage(mf)
		if err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, err
		}
		ret = append(ret, mf)
	}
	return ret, nil
}