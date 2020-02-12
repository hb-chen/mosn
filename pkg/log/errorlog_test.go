/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package log

import (
	"bufio"
	"os"
	"strings"
	"testing"
	"time"

	"mosn.io/pkg/log"
)

func readLines(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

// Errorf should add default error code
func TestErrorLog(t *testing.T) {
	logName := "/tmp/mosn/error_log_print.log"
	alogName := "/tmp/mosn/alert.error_log_print.log"
	os.Remove(logName)
	os.Remove(alogName)
	lg, err := GetOrCreateDefaultErrorLogger(logName, log.ERROR)
	if err != nil {
		t.Fatal("create logger failed")
	}
	lg.Errorf("testdata")
	lg.Alertf("mosn.test", "test_alert")
	time.Sleep(time.Second) // wait buffer flush
	// read lines
	if lines, err := readLines(logName); err != nil {
		t.Fatal(err)
	} else {
		if len(lines) != 1 {
			t.Fatalf("logger write lines not expected, writes: %d, expected: %d", len(lines), 1)
		}
		// verify log format
		// 2006-01-02 15:04:05,000 [ERROR] [normal] testdata
		out := strings.SplitN(lines[0], " ", 5)
		if !(len(out) == 5 &&
			out[2] == "[ERROR]" &&
			out[3] == "[normal]" &&
			out[4] == "testdata") {
			t.Errorf("output data is unexpected: %s", lines[0])
		}
	}
	// read alert log
	if lines, err := readLines(alogName); err != nil {
		t.Fatal(err)
	} else {
		if len(lines) != 1 {
			t.Fatalf("logger write lines not expected, writes: %d, expected: %d", len(lines), 1)
		}
		// 2006-01-02 15:04:05,000 [ERROR] [mosn.test] test_alert
		alert_out := strings.SplitN(lines[0], " ", 5)
		if !(len(alert_out) == 5 &&
			alert_out[2] == "[ERROR]" &&
			alert_out[3] == "[mosn.test]" &&
			alert_out[4] == "test_alert") {
			t.Errorf("output data is unexpected: %s", lines[1])
		}
	}
}
