"""Allow running as: python -m hive.mcp"""
from hive.mcp.server import run_server
import argparse

parser = argparse.ArgumentParser(description="HIVE MCP Server")
parser.add_argument("--db", default=None)
parser.add_argument("--channels", default=None)
args = parser.parse_args()
run_server(db_path=args.db, channels_dir=args.channels)
