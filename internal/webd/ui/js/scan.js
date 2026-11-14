// Reading a QR code with the device's camera, where the browser can
// (BarcodeDetector: Chrome on Android, the usual way to reach a node).
// Elsewhere the code is pasted instead.

export const canScan = () => 'BarcodeDetector' in window && !!(navigator.mediaDevices && navigator.mediaDevices.getUserMedia);
export const canScanImages = () => 'BarcodeDetector' in window && 'createImageBitmap' in window;

// startScan shows the camera in video and calls found(text) for the first
// QR code it sees. It returns a function that stops the camera.
export async function startScan(video, found) {
  const stream = await navigator.mediaDevices.getUserMedia({ video: { facingMode: { ideal: 'environment' } }, audio: false });
  video.srcObject = stream;
  video.setAttribute('playsinline', '');
  await video.play();
  const detector = new window.BarcodeDetector({ formats: ['qr_code'] });
  let stopped = false;
  const stop = () => {
    stopped = true;
    for (const t of stream.getTracks()) t.stop();
    video.srcObject = null;
  };
  const tick = async () => {
    if (stopped) return;
    try {
      const codes = await detector.detect(video);
      if (codes.length && codes[0].rawValue) {
        stop();
        found(codes[0].rawValue);
        return;
      }
    } catch (e) { /* a frame that could not be read: try the next */ }
    setTimeout(tick, 250);
  };
  tick();
  return stop;
}

// scanImage reads a QR code from a photo or screenshot.
export async function scanImage(file) {
  const bitmap = await createImageBitmap(file);
  const codes = await new window.BarcodeDetector({ formats: ['qr_code'] }).detect(bitmap);
  return codes.length ? codes[0].rawValue : null;
}
