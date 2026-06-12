"""Allow running as: python -m hive.mcp"""
import argparse

from hive.mcp.server import run_server

parser = argparse.ArgumentParser(description="HIVE MCP Server")
parser.add_argument("--db", default="C:/tools/agent-comms/hive.db")
parser.add_argument("--channels", default="C:/tools/agent-comms/channels")
args = parser.parse_args()
run_server(db_path=args.db, channels_dir=args.channels)
