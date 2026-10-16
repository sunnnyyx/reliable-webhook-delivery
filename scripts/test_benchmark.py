"""Analysis checks protect benchmark reporting, not application implementation."""
import unittest
from benchmark import percentiles


class AnalysisTests(unittest.TestCase):
    def test_empty_and_failed_cohorts_have_no_success_latency(self):
        self.assertEqual(percentiles([]), {'p50': None, 'p95': None, 'p99': None})

    def test_nearest_rank_keeps_small_sample_tail(self):
        self.assertEqual(percentiles([9, 1, 4, 2]), {'p50': 2, 'p95': 9, 'p99': 9})
        self.assertEqual(percentiles([3]), {'p50': 3, 'p95': 3, 'p99': 3})


if __name__ == '__main__':
    unittest.main()
