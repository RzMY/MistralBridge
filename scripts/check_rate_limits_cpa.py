"""Compatibility entry point for the native CPA scheduling and rate-limit checks."""
import argparse
from check_native_scheduler_cpa import run

if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--library", required=True)
    parser.add_argument("--version", default="0.4.0")
    parser.add_argument("--image", default="eceasy/cli-proxy-api:v8.1.2")
    parser.add_argument("--work-dir", required=True)
    run(parser.parse_args())
