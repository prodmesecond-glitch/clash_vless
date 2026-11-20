package com.clashvless;

import com.google.zxing.BarcodeFormat;
import com.google.zxing.common.BitMatrix;
import com.google.zxing.qrcode.QRCodeWriter;

import java.nio.charset.StandardCharsets;
import java.util.Base64;

/**
 * Host-side tests for QR decoding, scan cancellation and image sampling.
 */
public final class ScanImportTest {

    private static final String SAMPLE_SUB = "https://remnawave.example.com/api/v1/sub?token=happ_secret_token_99";
    private static final String SAMPLE_VLESS_1 = "vless://11111111-1111-1111-1111-111111111111@a.example:443?security=reality&sni=a.example&pbk=pubkey123&type=tcp#Exit1";
    private static final String SAMPLE_VLESS_2 = "vless://22222222-2222-2222-2222-222222222222@b.example:443?security=none&type=grpc&serviceName=svc#Exit2";
    private static final String SAMPLE_HWID = "e2b4f910-67a3-4819-b5d1-190f84a86c02";

    public static void main(String[] args) throws Exception {
        System.out.println("Starting ScanImportTest...");

        testSubscriptionQRExportAndScan();
        testExitRouteSingleQRExportAndScan();
        testExitRouteMultiQRExportAndScan();
        testExitRouteBase64QRExportAndScan();
        testHWIDQRExportAndScan();
        testCameraPreviewYUVAllOrientations();
        testCorruptedDataGracefulNull();
        testCancelledScanDropsQueuedResult();
        testImageSamplingBounds();

        System.out.println("ALL SCAN & IMPORT TESTS PASSED!");
    }

    private static void assertEquals(String expected, String actual, String msg) {
        if (!expected.equals(actual)) {
            throw new AssertionError(msg + " - Expected: " + expected + ", Actual: " + actual);
        }
    }

    private static void assertTrue(boolean condition, String msg) {
        if (!condition) {
            throw new AssertionError(msg);
        }
    }

    private static int[] generateQRPixels(String text, int size) throws Exception {
        QRCodeWriter writer = new QRCodeWriter();
        BitMatrix matrix = writer.encode(text, BarcodeFormat.QR_CODE, size, size);
        int w = matrix.getWidth(), h = matrix.getHeight();
        int[] pixels = new int[w * h];
        for (int y = 0; y < h; y++) {
            for (int x = 0; x < w; x++) {
                pixels[y * w + x] = matrix.get(x, y) ? 0xFF000000 : 0xFFFFFFFF;
            }
        }
        return pixels;
    }

    private static byte[] pixelsToNV21(int[] pixels, int width, int height) {
        byte[] yuv = new byte[width * height * 3 / 2];
        for (int i = 0; i < width * height; i++) {
            int c = pixels[i];
            int r = (c >> 16) & 0xFF;
            int g = (c >> 8) & 0xFF;
            int b = c & 0xFF;
            // RGB to Y (luminance)
            int y = (66 * r + 129 * g + 25 * b + 128) >> 8;
            yuv[i] = (byte) Math.min(255, Math.max(0, y + 16));
        }
        // Fill UV plane with neutral 128
        for (int i = width * height; i < yuv.length; i++) {
            yuv[i] = (byte) 128;
        }
        return yuv;
    }

    private static void testSubscriptionQRExportAndScan() throws Exception {
        System.out.println("Testing Subscription QR Export and Scan...");
        int size = 240;
        int[] pixels = generateQRPixels(SAMPLE_SUB, size);
        String decoded = QrScanner.decodePixels(pixels, size, size);
        assertEquals(SAMPLE_SUB, decoded, "Subscription URL QR scan mismatch");
        assertTrue(decoded.startsWith("https://") || decoded.startsWith("http://"), "Must be valid http(s) URL");
        System.out.println("  PASSED: Subscription QR scan decoded correctly: " + decoded);
    }

    private static void testExitRouteSingleQRExportAndScan() throws Exception {
        System.out.println("Testing Single Exit Route QR Export and Scan...");
        int size = 260;
        int[] pixels = generateQRPixels(SAMPLE_VLESS_1, size);
        String decoded = QrScanner.decodePixels(pixels, size, size);
        assertEquals(SAMPLE_VLESS_1, decoded, "Exit route VLESS link QR scan mismatch");
        assertTrue(decoded.startsWith("vless://"), "Must start with vless://");
        System.out.println("  PASSED: Single exit route QR scan decoded correctly: " + decoded);
    }

    private static void testExitRouteMultiQRExportAndScan() throws Exception {
        System.out.println("Testing Multi-Exit Route QR Export and Scan...");
        String joined = SAMPLE_VLESS_1 + "\n" + SAMPLE_VLESS_2;
        int size = 300;
        int[] pixels = generateQRPixels(joined, size);
        String decoded = QrScanner.decodePixels(pixels, size, size);
        assertEquals(joined, decoded, "Multi-exit QR scan mismatch");
        String[] lines = decoded.split("\\r?\\n");
        assertEquals(String.valueOf(2), String.valueOf(lines.length), "Expected 2 exit lines");
        assertEquals(SAMPLE_VLESS_1, lines[0], "First exit mismatch");
        assertEquals(SAMPLE_VLESS_2, lines[1], "Second exit mismatch");
        System.out.println("  PASSED: Multi-exit QR scan decoded correctly");
    }

    private static void testExitRouteBase64QRExportAndScan() throws Exception {
        System.out.println("Testing Base64 Subscription Exit Route QR Export and Scan...");
        String joined = SAMPLE_VLESS_1 + "\n" + SAMPLE_VLESS_2;
        String b64 = Base64.getEncoder().encodeToString(joined.getBytes(StandardCharsets.UTF_8));
        int size = 300;
        int[] pixels = generateQRPixels(b64, size);
        String decoded = QrScanner.decodePixels(pixels, size, size);
        assertEquals(b64, decoded, "Base64 exit QR scan mismatch");

        byte[] unb64 = Base64.getDecoder().decode(decoded);
        String text = new String(unb64, StandardCharsets.UTF_8);
        assertEquals(joined, text, "Unbased text mismatch");
        System.out.println("  PASSED: Base64 exit route QR scan decoded and unpacked correctly");
    }

    private static void testHWIDQRExportAndScan() throws Exception {
        System.out.println("Testing HWID QR Export and Scan...");
        int size = 200;
        int[] pixels = generateQRPixels(SAMPLE_HWID, size);
        String decoded = QrScanner.decodePixels(pixels, size, size);
        assertEquals(SAMPLE_HWID, decoded, "HWID QR scan mismatch");
        assertTrue(decoded.matches("[a-zA-Z0-9=-]{10,64}"), "HWID format validation must pass");
        System.out.println("  PASSED: HWID QR scan decoded correctly: " + decoded);
    }

    private static void testCameraPreviewYUVAllOrientations() throws Exception {
        System.out.println("Testing Camera Preview YUV Frame decoding across all orientations...");
        int size = 240;
        int[] pixels = generateQRPixels(SAMPLE_SUB, size);
        byte[] yuv = pixelsToNV21(pixels, size, size);

        // 0 degrees
        String d0 = QrScanner.decodeYUV(yuv, size, size, 0);
        assertEquals(SAMPLE_SUB, d0, "YUV 0° decoding failed");

        // 90 degrees
        byte[] yuv90 = new byte[yuv.length];
        for (int y = 0; y < size; y++) {
            for (int x = 0; x < size; x++) {
                yuv90[x * size + (size - y - 1)] = yuv[y * size + x];
            }
        }
        String d90 = QrScanner.decodeYUV(yuv90, size, size, 90);
        assertEquals(SAMPLE_SUB, d90, "YUV 90° decoding failed");

        // 180 degrees
        byte[] yuv180 = new byte[yuv.length];
        for (int i = 0; i < size * size; i++) {
            yuv180[size * size - 1 - i] = yuv[i];
        }
        String d180 = QrScanner.decodeYUV(yuv180, size, size, 180);
        assertEquals(SAMPLE_SUB, d180, "YUV 180° decoding failed");

        // 270 degrees
        byte[] yuv270 = new byte[yuv.length];
        for (int y = 0; y < size; y++) {
            for (int x = 0; x < size; x++) {
                yuv270[(size - x - 1) * size + y] = yuv[y * size + x];
            }
        }
        String d270 = QrScanner.decodeYUV(yuv270, size, size, 270);
        assertEquals(SAMPLE_SUB, d270, "YUV 270° decoding failed");

        System.out.println("  PASSED: Camera preview YUV decoding works in all 4 orientations (0°, 90°, 180°, 270°)");
    }

    private static void testCorruptedDataGracefulNull() {
        System.out.println("Testing corrupted/empty data graceful handling...");
        assertTrue(QrScanner.decodePixels(null, 10, 10) == null, "Null pixels should return null");
        assertTrue(QrScanner.decodePixels(new int[100], 10, 10) == null, "Blank pixels should return null");
        assertTrue(QrScanner.decodeYUV(null, 10, 10, 0) == null, "Null YUV should return null");
        assertTrue(QrScanner.decodeYUV(new byte[100], 10, 10, 0) == null, "Blank YUV should return null");
        assertTrue(QrScanner.decodePixels(new int[10], Integer.MAX_VALUE, Integer.MAX_VALUE) == null, "Overflowing dimensions must be rejected");
        assertTrue(QrScanner.decodeYUV(new byte[10], 100, 100, 90) == null, "Truncated YUV must be rejected before rotation");
        System.out.println("  PASSED: Corrupted/empty data gracefully handled without exceptions");
    }

    private static void testCancelledScanDropsQueuedResult() {
        QrScanner.ScanSession cancelled = new QrScanner.ScanSession();
        assertTrue(cancelled.queueResult(), "A decoded result must be queued");
        cancelled.cancel();
        assertTrue(!cancelled.deliverResult(), "Cancel must discard a result already queued on the UI thread");
        assertTrue(!cancelled.isActive() && !cancelled.queueResult(), "Cancelled scanners must reject later frames");

        QrScanner.ScanSession closed = new QrScanner.ScanSession();
        closed.cancel();
        assertTrue(!closed.queueResult(), "Closing before decoding must discard the result");

        QrScanner.ScanSession success = new QrScanner.ScanSession();
        assertTrue(success.queueResult(), "A live scan must accept its first result");
        assertTrue(!success.queueResult(), "A second frame must not enqueue another result");
        assertTrue(success.deliverResult(), "A live queued result must be delivered");
        assertTrue(!success.deliverResult(), "A result must be delivered only once");
        success.cancel();
        System.out.println("  PASSED: Scan cancellation drops queued results; successful scans deliver once");
    }

    private static void testImageSamplingBounds() {
        int[][] cases = {{240, 240, 1}, {2048, 2048, 1}, {2049, 2048, 2},
                {8000, 6000, 4}, {6000, 8000, 4}, {12000, 9000, 8}, {1, Integer.MAX_VALUE, 1048576}};
        for (int[] dims : cases) {
            int sample = QrScanner.imageSampleSize(dims[0], dims[1]);
            assertTrue(sample == dims[2], "Unexpected image downsampling factor");
            long width = ((long) dims[0] + sample - 1) / sample;
            long height = ((long) dims[1] + sample - 1) / sample;
            assertTrue(width <= 2048 && height <= 2048, "A decoded image must fit the memory bound");
        }
        System.out.println("  PASSED: Large images are sampled before allocation, including 48 MP and portrait photos");
    }
}
