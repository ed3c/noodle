#!/usr/bin/env python3
"""Evaluate frozen reports or retained bills without model/owner operations."""
import argparse
import json
from pathlib import Path

from independent_evaluation import (
    ReviewedRun,
    accounting,
    aggregate_pairs,
    judge_pair,
    load_native_judge,
)
from soodles_bridge import decode, require, save


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('operation', choices=('pair', 'accounting', 'aggregate'))
    parser.add_argument('config', type=Path)
    parser.add_argument('output', type=Path)
    args = parser.parse_args()
    config = decode(args.config.read_bytes())
    require(config.get('status') == 'ready', 'evaluation inputs remain incomplete')
    if args.operation == 'pair':
        result = judge_pair(rubric=config['rubric'], baseline=ReviewedRun(**config['baseline']),
            candidate=ReviewedRun(**config['candidate']), selection=config['selection'],
            native_judge=load_native_judge(config['upstream']))
    elif args.operation == 'accounting':
        result = accounting(config['journals'], external_calls=config['external_calls'],
                            wall_interval=config['wall_interval'])
    else:
        result = aggregate_pairs(config['reports'])
    print(json.dumps(save(args.output.resolve(), result)))


if __name__ == '__main__':
    main()
