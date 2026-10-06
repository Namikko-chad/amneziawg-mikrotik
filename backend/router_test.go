package main

import "testing"

func TestParseDefaultGateway(t *testing.T) {
	header := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"
	local := "veth-awg\t000012AC\t00000000\t0001\t0\t0\t0\tFCFFFFFF\t0\t0\t0\n"
	table := header + local + "veth-awg\t00000000\t010012AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"
	gw, err := parseDefaultGateway(table)
	if err != nil || gw.String() != "172.18.0.1" {
		t.Fatalf("gw = %v, %v", gw, err)
	}
	if _, err := parseDefaultGateway(header + local); err == nil {
		t.Fatal("expected error")
	}
}
