"""Regression tests for the asset set published by .github/workflows/release.yml."""

import unittest

from release_assets import expected_assets, verify_assets


class ReleaseAssetTests(unittest.TestCase):
    def test_full_release_has_platform_reauth_and_prism_assets(self):
        actual = expected_assets("0.2.32")
        self.assertEqual(len(actual), 10)
        self.assertIn("checksums.txt", actual)
        self.assertIn("sub2api-reauth_0.2.32_linux_amd64.tar.gz", actual)
        self.assertIn("sub2api-reauth_0.2.32_linux_arm64.tar.gz", actual)
        self.assertIn("prism-browser_0.2.32.tar.gz", actual)
        self.assertIn("prism-browser_0.2.32.tar.gz.sha256", actual)

    def test_missing_or_unexpected_asset_fails(self):
        actual = expected_assets("0.2.32")
        actual.remove("prism-browser_0.2.32.tar.gz")
        with self.assertRaisesRegex(ValueError, "missing"):
            verify_assets("0.2.32", actual)

        actual = expected_assets("0.2.32") | {"old-debug-log.txt"}
        with self.assertRaisesRegex(ValueError, "unexpected"):
            verify_assets("0.2.32", actual)

    def test_duplicate_asset_name_fails(self):
        actual = list(expected_assets("0.2.32"))
        actual.append(actual[0])
        with self.assertRaisesRegex(ValueError, "duplicate"):
            verify_assets("0.2.32", actual)


if __name__ == "__main__":
    unittest.main()
