package com.clashvless;

import android.app.Activity;
import android.app.Dialog;
import android.content.Context;
import android.content.ContentResolver;
import android.content.Intent;
import android.graphics.Bitmap;
import android.graphics.BitmapFactory;
import android.graphics.Canvas;
import android.graphics.Color;
import android.graphics.Paint;
import android.graphics.Rect;
import android.graphics.SurfaceTexture;
import android.graphics.drawable.ColorDrawable;
import android.graphics.drawable.GradientDrawable;
import android.hardware.Camera;
import android.net.Uri;
import android.view.Gravity;
import android.view.HapticFeedbackConstants;
import android.view.KeyEvent;
import android.view.Surface;
import android.view.TextureView;
import android.view.View;
import android.view.Window;
import android.widget.Button;
import android.widget.FrameLayout;
import android.widget.LinearLayout;
import android.widget.TextView;

import com.google.zxing.BinaryBitmap;
import com.google.zxing.PlanarYUVLuminanceSource;
import com.google.zxing.RGBLuminanceSource;
import com.google.zxing.common.GlobalHistogramBinarizer;
import com.google.zxing.common.HybridBinarizer;
import com.google.zxing.qrcode.QRCodeReader;

import java.io.IOException;
import java.io.InputStream;
import java.util.List;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.RejectedExecutionException;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.function.Consumer;

/**
 * Real-time camera viewfinder and image scanner for QR codes using ZXing.
 */
public final class QrScanner {
    private static final int TEXT = 0xFFdee6ef;
    private static final int MUTED = 0xFF919dab;
    private static final int CARD = 0xFF1a222d;
    private static final int BORDER = 0xFF2b3541;
    private static final int ACCENT = 0xFF00cc99;
    private static final int CONTROL = 0xFF1f2731;
    private static final int BLACK = 0xFF000000;

    public static final int REQ_PICK_IMAGE = 2001;
    private static final int MAX_IMAGE_SIDE = 2048;

    private QrScanner() { }

    static int imageSampleSize(int width, int height) {
        if (width <= 0 || height <= 0) throw new IllegalArgumentException("Invalid image dimensions");
        int sample = 1;
        while ((width - 1) / sample + 1 > MAX_IMAGE_SIDE || (height - 1) / sample + 1 > MAX_IMAGE_SIDE) {
            sample *= 2;
        }
        return sample;
    }

    public static String decodeImage(ContentResolver resolver, Uri uri) throws IOException {
        BitmapFactory.Options options = new BitmapFactory.Options();
        options.inJustDecodeBounds = true;
        try (InputStream stream = resolver.openInputStream(uri)) {
            BitmapFactory.decodeStream(stream, null, options);
        }
        if (options.outWidth <= 0 || options.outHeight <= 0) return null;
        options.inSampleSize = imageSampleSize(options.outWidth, options.outHeight);
        options.inJustDecodeBounds = false;
        Bitmap bitmap = null;
        try (InputStream stream = resolver.openInputStream(uri)) {
            bitmap = BitmapFactory.decodeStream(stream, null, options);
            return decodeBitmap(bitmap);
        } finally {
            if (bitmap != null) bitmap.recycle();
        }
    }

    /** Decodes a QR code from a standard RGB Bitmap. */
    public static String decodeBitmap(Bitmap bitmap) {
        if (bitmap == null) return null;
        int width = bitmap.getWidth();
        int height = bitmap.getHeight();
        Bitmap scaled = bitmap;
        try {
            if (width > MAX_IMAGE_SIDE || height > MAX_IMAGE_SIDE) {
                double ratio = (double) MAX_IMAGE_SIDE / Math.max(width, height);
                width = Math.max(1, (int) (width * ratio));
                height = Math.max(1, (int) (height * ratio));
                scaled = Bitmap.createScaledBitmap(bitmap, width, height, true);
            }
            int[] pixels = new int[width * height];
            scaled.getPixels(pixels, 0, width, 0, 0, width, height);
            return decodePixels(pixels, width, height);
        } finally {
            if (scaled != bitmap) scaled.recycle();
        }
    }

    /** Decodes a QR code from raw ARGB/RGB pixel array. */
    public static String decodePixels(int[] pixels, int width, int height) {
        if (pixels == null || width <= 0 || height <= 0 || pixels.length < (long) width * height) return null;
        RGBLuminanceSource source = new RGBLuminanceSource(width, height, pixels);
        QRCodeReader reader = new QRCodeReader();
        try {
            return reader.decode(new BinaryBitmap(new HybridBinarizer(source))).getText();
        } catch (Exception e1) {
            try {
                return reader.decode(new BinaryBitmap(new GlobalHistogramBinarizer(source))).getText();
            } catch (Exception e2) {
                return null;
            }
        }
    }

    /** Decodes a QR code from camera preview NV21 byte array with display orientation correction. */
    public static String decodeYUV(byte[] data, int width, int height, int orientation) {
        if (data == null || width <= 0 || height <= 0 || data.length < (long) width * height) return null;
        byte[] luminance;
        int lumWidth, lumHeight;

        if (orientation == 90) {
            lumWidth = height;
            lumHeight = width;
            luminance = new byte[lumWidth * lumHeight];
            for (int y = 0; y < height; y++) {
                for (int x = 0; x < width; x++) {
                    luminance[x * height + (height - y - 1)] = data[x + y * width];
                }
            }
        } else if (orientation == 270) {
            lumWidth = height;
            lumHeight = width;
            luminance = new byte[lumWidth * lumHeight];
            for (int y = 0; y < height; y++) {
                for (int x = 0; x < width; x++) {
                    luminance[(width - x - 1) * height + y] = data[x + y * width];
                }
            }
        } else if (orientation == 180) {
            lumWidth = width;
            lumHeight = height;
            luminance = new byte[lumWidth * lumHeight];
            int total = width * height;
            for (int i = 0; i < total; i++) {
                luminance[total - 1 - i] = data[i];
            }
        } else {
            lumWidth = width;
            lumHeight = height;
            luminance = data;
        }

        PlanarYUVLuminanceSource source = new PlanarYUVLuminanceSource(
                luminance, lumWidth, lumHeight, 0, 0, lumWidth, lumHeight, false);
        QRCodeReader reader = new QRCodeReader();
        try {
            return reader.decode(new BinaryBitmap(new HybridBinarizer(source))).getText();
        } catch (Exception e1) {
            try {
                return reader.decode(new BinaryBitmap(new GlobalHistogramBinarizer(source))).getText();
            } catch (Exception e2) {
                return null;
            }
        }
    }

    /** Launches the system image picker to choose a QR code image from storage. */
    public static void pickImage(Activity activity) {
        Intent intent = new Intent(Intent.ACTION_GET_CONTENT);
        intent.setType("image/*");
        activity.startActivityForResult(Intent.createChooser(intent, "Choose QR Code image"), REQ_PICK_IMAGE);
    }

    /** Opens an in-app live camera viewfinder dialog to scan a QR code. */
    public static void showScanner(Activity activity, String title, Runnable onPickImage, Consumer<String> onResult) {
        Dialog dialog = new Dialog(activity);
        dialog.requestWindowFeature(Window.FEATURE_NO_TITLE);
        dialog.setOnKeyListener((d, keyCode, event) -> {
            if (keyCode == KeyEvent.KEYCODE_ESCAPE && event.getAction() == KeyEvent.ACTION_UP) {
                dialog.dismiss();
                return true;
            }
            return false;
        });

        int dp1 = (int) (activity.getResources().getDisplayMetrics().density + 0.5f);
        int dialogWidth = Math.min((int) (activity.getResources().getDisplayMetrics().widthPixels * 0.92f), dp1 * 400);

        LinearLayout root = new LinearLayout(activity);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setPadding(dp1 * 18, dp1 * 16, dp1 * 18, dp1 * 16);
        GradientDrawable bg = new GradientDrawable();
        bg.setColor(CARD);
        bg.setCornerRadius(dp1 * 12);
        bg.setStroke(dp1, BORDER);
        root.setBackground(bg);

        TextView titleView = new TextView(activity);
        titleView.setText(title);
        titleView.setTextSize(16);
        titleView.setTextColor(TEXT);
        titleView.setTypeface(null, android.graphics.Typeface.BOLD);
        root.addView(titleView);

        TextView hintView = new TextView(activity);
        hintView.setText("Align QR code within the frame or choose an image");
        hintView.setTextSize(12);
        hintView.setTextColor(MUTED);
        LinearLayout.LayoutParams hintParams = new LinearLayout.LayoutParams(-1, -2);
        hintParams.setMargins(0, dp1 * 4, 0, dp1 * 12);
        root.addView(hintView, hintParams);

        int previewSide = Math.min(dialogWidth - dp1 * 36, dp1 * 280);
        FrameLayout previewContainer = new FrameLayout(activity);
        GradientDrawable containerBg = new GradientDrawable();
        containerBg.setColor(BLACK);
        containerBg.setCornerRadius(dp1 * 8);
        previewContainer.setBackground(containerBg);
        previewContainer.setClipToOutline(true);

        TextureView textureView = new TextureView(activity);
        previewContainer.addView(textureView, new FrameLayout.LayoutParams(-1, -1));

        View reticleView = new ReticleView(activity, dp1);
        previewContainer.addView(reticleView, new FrameLayout.LayoutParams(-1, -1));

        TextView errorView = new TextView(activity);
        errorView.setText("Camera preview unavailable\nUse \"Choose image\" below");
        errorView.setTextColor(MUTED);
        errorView.setTextSize(13);
        errorView.setGravity(Gravity.CENTER);
        errorView.setVisibility(View.GONE);
        previewContainer.addView(errorView, new FrameLayout.LayoutParams(-1, -1));

        root.addView(previewContainer, new LinearLayout.LayoutParams(previewSide, previewSide));

        LinearLayout buttons = new LinearLayout(activity);
        buttons.setOrientation(LinearLayout.HORIZONTAL);
        buttons.setGravity(Gravity.END);
        LinearLayout.LayoutParams btnParams = new LinearLayout.LayoutParams(-1, -2);
        btnParams.setMargins(0, dp1 * 14, 0, 0);

        Button pickBtn = makeButton(activity, "Choose image", false, dp1, () -> {
            dialog.dismiss();
            if (onPickImage != null) onPickImage.run();
        });
        Button cancelBtn = makeButton(activity, "Cancel", false, dp1, dialog::dismiss);

        buttons.addView(pickBtn);
        View spacer = new View(activity);
        buttons.addView(spacer, new LinearLayout.LayoutParams(dp1 * 8, 1));
        buttons.addView(cancelBtn);
        root.addView(buttons, btnParams);

        dialog.setContentView(root);
        if (dialog.getWindow() != null) {
            dialog.getWindow().setBackgroundDrawable(new ColorDrawable(Color.TRANSPARENT));
        }

        CameraHolder holder = new CameraHolder(activity, textureView, errorView, onResult, dialog);
        dialog.setOnDismissListener(d -> holder.release());
        dialog.show();
    }

    private static Button makeButton(Context context, String label, boolean primary, int dp1, Runnable action) {
        Button button = new Button(context);
        button.setText(label);
        button.setTextSize(13);
        button.setTextColor(primary ? BLACK : TEXT);
        button.setTypeface(null, android.graphics.Typeface.BOLD);
        GradientDrawable d = new GradientDrawable();
        d.setColor(primary ? ACCENT : CONTROL);
        d.setCornerRadius(dp1 * 6);
        d.setStroke(dp1, primary ? ACCENT : BORDER);
        button.setBackground(d);
        button.setPadding(dp1 * 14, 0, dp1 * 14, 0);
        button.setOnClickListener(v -> action.run());
        return button;
    }

    private static class ReticleView extends View {
        private final int dp1;
        private final Paint borderPaint;
        private final Paint scrimPaint;

        ReticleView(Context context, int dp1) {
            super(context);
            this.dp1 = dp1;
            borderPaint = new Paint(Paint.ANTI_ALIAS_FLAG);
            borderPaint.setColor(ACCENT);
            borderPaint.setStyle(Paint.Style.STROKE);
            borderPaint.setStrokeWidth(dp1 * 3);

            scrimPaint = new Paint();
            scrimPaint.setColor(0x64000000);
        }

        @Override
        protected void onDraw(Canvas canvas) {
            int w = getWidth(), h = getHeight();
            int boxSide = (int) (Math.min(w, h) * 0.72f);
            int left = (w - boxSide) / 2;
            int top = (h - boxSide) / 2;
            int right = left + boxSide;
            int bottom = top + boxSide;

            // Draw scrim around the viewfinder
            canvas.drawRect(0, 0, w, top, scrimPaint);
            canvas.drawRect(0, bottom, w, h, scrimPaint);
            canvas.drawRect(0, top, left, bottom, scrimPaint);
            canvas.drawRect(right, top, w, bottom, scrimPaint);

            // Draw corner reticles
            int cornerLen = dp1 * 20;
            // Top-left
            canvas.drawLine(left, top, left + cornerLen, top, borderPaint);
            canvas.drawLine(left, top, left, top + cornerLen, borderPaint);
            // Top-right
            canvas.drawLine(right, top, right - cornerLen, top, borderPaint);
            canvas.drawLine(right, top, right, top + cornerLen, borderPaint);
            // Bottom-left
            canvas.drawLine(left, bottom, left + cornerLen, bottom, borderPaint);
            canvas.drawLine(left, bottom, left, bottom - cornerLen, borderPaint);
            // Bottom-right
            canvas.drawLine(right, bottom, right - cornerLen, bottom, borderPaint);
            canvas.drawLine(right, bottom, right, bottom - cornerLen, borderPaint);
        }
    }

    static final class ScanSession {
        private int state;

        synchronized boolean isActive() { return state == 0; }

        synchronized boolean queueResult() {
            if (state != 0) return false;
            state = 1;
            return true;
        }

        synchronized boolean deliverResult() {
            if (state != 1) return false;
            state = 2;
            return true;
        }

        synchronized void cancel() { state = 3; }
    }

    private static class CameraHolder implements TextureView.SurfaceTextureListener {
        private final Activity activity;
        private final TextureView textureView;
        private final TextView errorView;
        private final Consumer<String> onResult;
        private final Dialog dialog;
        private final ExecutorService executor = Executors.newSingleThreadExecutor();
        private final AtomicBoolean isAnalyzing = new AtomicBoolean(false);
        private final ScanSession session = new ScanSession();
        private Camera camera;
        private int orientationDegrees = 0;
        private int previewWidth = 0;
        private int previewHeight = 0;

        CameraHolder(Activity activity, TextureView textureView, TextView errorView, Consumer<String> onResult, Dialog dialog) {
            this.activity = activity;
            this.textureView = textureView;
            this.errorView = errorView;
            this.onResult = onResult;
            this.dialog = dialog;
            textureView.setSurfaceTextureListener(this);
        }

        @Override
        public void onSurfaceTextureAvailable(SurfaceTexture surface, int width, int height) {
            startCamera(surface);
        }

        @Override
        public void onSurfaceTextureSizeChanged(SurfaceTexture surface, int width, int height) { }

        @Override
        public boolean onSurfaceTextureDestroyed(SurfaceTexture surface) {
            stopCamera();
            return true;
        }

        @Override
        public void onSurfaceTextureUpdated(SurfaceTexture surface) { }

        private void startCamera(SurfaceTexture surface) {
            new Thread(() -> openCamera(surface)).start();
        }

        private synchronized void openCamera(SurfaceTexture surface) {
            if (!session.isActive()) return;
            int count = Camera.getNumberOfCameras();
            int cameraId = -1;
            Camera.CameraInfo info = new Camera.CameraInfo();
            for (int i = 0; i < count; i++) {
                Camera.getCameraInfo(i, info);
                if (info.facing == Camera.CameraInfo.CAMERA_FACING_BACK) {
                    cameraId = i;
                    break;
                }
            }
            if (cameraId == -1 && count > 0) cameraId = 0;

            try {
                camera = cameraId >= 0 ? Camera.open(cameraId) : Camera.open();
            } catch (Exception e) {
                camera = null;
            }

            if (camera == null) {
                activity.runOnUiThread(() -> errorView.setVisibility(View.VISIBLE));
                return;
            }

            try {
                int rotation = activity.getWindowManager().getDefaultDisplay().getRotation();
                int degrees = 0;
                switch (rotation) {
                    case Surface.ROTATION_0: degrees = 0; break;
                    case Surface.ROTATION_90: degrees = 90; break;
                    case Surface.ROTATION_180: degrees = 180; break;
                    case Surface.ROTATION_270: degrees = 270; break;
                }
                if (cameraId >= 0) {
                    Camera.getCameraInfo(cameraId, info);
                    orientationDegrees = (info.orientation - degrees + 360) % 360;
                } else {
                    orientationDegrees = 90;
                }
                camera.setDisplayOrientation(orientationDegrees);

                Camera.Parameters params = camera.getParameters();
                List<String> focusModes = params.getSupportedFocusModes();
                if (focusModes != null && focusModes.contains(Camera.Parameters.FOCUS_MODE_CONTINUOUS_PICTURE)) {
                    params.setFocusMode(Camera.Parameters.FOCUS_MODE_CONTINUOUS_PICTURE);
                } else if (focusModes != null && focusModes.contains(Camera.Parameters.FOCUS_MODE_AUTO)) {
                    params.setFocusMode(Camera.Parameters.FOCUS_MODE_AUTO);
                }

                List<Camera.Size> sizes = params.getSupportedPreviewSizes();
                Camera.Size optimal = null;
                int minDiff = Integer.MAX_VALUE;
                for (Camera.Size s : sizes) {
                    int diff = Math.abs(s.width - 1280) + Math.abs(s.height - 720);
                    if (diff < minDiff) {
                        minDiff = diff;
                        optimal = s;
                    }
                }
                if (optimal != null) {
                    params.setPreviewSize(optimal.width, optimal.height);
                    previewWidth = optimal.width;
                    previewHeight = optimal.height;
                } else {
                    Camera.Size cur = params.getPreviewSize();
                    previewWidth = cur.width;
                    previewHeight = cur.height;
                }

                camera.setParameters(params);
                camera.setPreviewTexture(surface);
                camera.setPreviewCallback((data, cam) -> {
                    if (!session.isActive() || !isAnalyzing.compareAndSet(false, true)) return;
                    try {
                        executor.execute(() -> {
                            try {
                                String text = decodeYUV(data, previewWidth, previewHeight, orientationDegrees);
                                if (text != null && !text.trim().isEmpty()) {
                                    if (session.queueResult()) {
                                        activity.runOnUiThread(() -> {
                                            if (!dialog.isShowing() || activity.isFinishing() || activity.isDestroyed()) return;
                                            if (!session.deliverResult()) return;
                                            textureView.performHapticFeedback(HapticFeedbackConstants.KEYBOARD_TAP);
                                            dialog.dismiss();
                                            onResult.accept(text.trim());
                                        });
                                    }
                                }
                            } catch (Exception ignored) {
                            } finally {
                                isAnalyzing.set(false);
                            }
                        });
                    } catch (RejectedExecutionException ignored) {
                        isAnalyzing.set(false);
                    }
                });
                camera.startPreview();
            } catch (Exception e) {
                stopCamera();
                activity.runOnUiThread(() -> errorView.setVisibility(View.VISIBLE));
            }
        }

        private synchronized void stopCamera() {
            if (camera != null) {
                try {
                    camera.setPreviewCallback(null);
                    camera.stopPreview();
                    camera.release();
                } catch (Exception ignored) { }
                camera = null;
            }
        }

        synchronized void release() {
            session.cancel();
            stopCamera();
            executor.shutdownNow();
        }
    }
}
