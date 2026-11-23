package io.homing.client

import android.content.Context
import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.net.Uri
import com.google.zxing.BarcodeFormat
import com.google.zxing.BinaryBitmap
import com.google.zxing.DecodeHintType
import com.google.zxing.multi.GenericMultipleBarcodeReader
import com.google.zxing.MultiFormatReader
import com.google.zxing.NotFoundException
import com.google.zxing.RGBLuminanceSource
import com.google.zxing.common.GlobalHistogramBinarizer
import com.google.zxing.common.HybridBinarizer
import java.io.File
import java.io.InputStream

/**
 * 相册图片二维码解码（2.3.4）：
 *
 * 有些场景拿不到相机——二维码是管理页截图、别人发来的图片、或者用户把 PNG 存进了相册。
 * 这里用 ZXing core 直接对相册图片做识别，不依赖相机：
 *
 *  - 超大图按采样缩到 2048px 内（省内存，不影响二维码识别）；
 *  - 小图（截图里二维码只占几百像素）额外放大 2 倍再试，给密集二维码留够像素；
 *  - 每张图依次尝试 HybridBinarizer / GlobalHistogramBinarizer，以及反色版本；
 *  - 单码失败后用「多二维码」模式兜底（照片里二维码只占一角时更容易成功）。
 *
 * 解码是纯 CPU 操作，调用方需放在后台线程。
 */
object QrImageDecoder {

    private const val MAX_DIMENSION = 2048
    private const val UPSCALE_MAX_DIMENSION = 800

    sealed class Result {
        data class Success(val text: String) : Result()
        data class Failure(val reason: String) : Result()
    }

    fun decode(context: Context, uri: Uri): Result {
        val bitmap = loadBitmap(context, uri)
            ?: return Result.Failure("图片读取失败，请换一张图片试试")
        return decodeBitmap(bitmap)
    }

    private fun openStream(context: Context, uri: Uri): InputStream? =
        when (uri.scheme?.lowercase()) {
            // 自动化测试 / 调试钩子会传 file:// Uri；直接读文件，不走 ContentResolver
            "file" -> uri.path?.let { path ->
                runCatching { File(path).inputStream() }
                    .onFailure { HomingState.log("读取相册文件失败（$path）：$it") }
                    .getOrNull()
            }
            else -> context.contentResolver.openInputStream(uri)
        }

    private fun loadBitmap(context: Context, uri: Uri): Bitmap? {
        return try {
            val bounds = BitmapFactory.Options().apply { inJustDecodeBounds = true }
            openStream(context, uri)?.use { stream ->
                BitmapFactory.decodeStream(stream, null, bounds)
            }
            if (bounds.outWidth <= 0 || bounds.outHeight <= 0) {
                HomingState.log("相册图片解码失败：无法读取图片尺寸（${uri.scheme}）")
                return null
            }

            var sample = 1
            while (maxOf(bounds.outWidth, bounds.outHeight) / (sample * 2) >= MAX_DIMENSION) {
                sample *= 2
            }
            val options = BitmapFactory.Options().apply {
                inSampleSize = sample
                inPreferredConfig = Bitmap.Config.ARGB_8888
            }
            openStream(context, uri)?.use { stream ->
                BitmapFactory.decodeStream(stream, null, options)
            }
        } catch (t: Throwable) {
            HomingState.log("相册图片读取异常：$t")
            null
        }
    }

    private fun decodeBitmap(bitmap: Bitmap): Result {
        if (bitmap.width <= 0 || bitmap.height <= 0) {
            return Result.Failure("图片尺寸无效")
        }
        // 原图先试；小图再放大 2 倍试一次（截图里的二维码模块可能只有 1~2 像素）
        val candidates = mutableListOf(bitmap)
        val maxDim = maxOf(bitmap.width, bitmap.height)
        if (maxDim <= UPSCALE_MAX_DIMENSION) {
            runCatching {
                Bitmap.createScaledBitmap(bitmap, bitmap.width * 2, bitmap.height * 2, true)
            }.getOrNull()?.let { if (it !== bitmap) candidates += it }
        }

        for (candidate in candidates) {
            decodeOne(candidate)?.let { return Result.Success(it) }
        }
        return Result.Failure("图片里没有识别到二维码，请确认图片清晰完整")
    }

    /** 对单张位图依次尝试各种二值化/反色/多码组合，成功返回二维码文本。 */
    private fun decodeOne(bitmap: Bitmap): String? {
        val width = bitmap.width
        val height = bitmap.height
        val pixels = IntArray(width * height)
        bitmap.getPixels(pixels, 0, width, 0, 0, width, height)
        val source = RGBLuminanceSource(width, height, pixels)

        val hints = mapOf(
            DecodeHintType.POSSIBLE_FORMATS to listOf(BarcodeFormat.QR_CODE),
            DecodeHintType.TRY_HARDER to true,
            DecodeHintType.CHARACTER_SET to "UTF-8",
        )

        // 普通/反色 × 两种二值化，覆盖截图、照片、深色底二维码
        val binarizers = listOf(
            HybridBinarizer(source),
            GlobalHistogramBinarizer(source),
            HybridBinarizer(source.invert()),
            GlobalHistogramBinarizer(source.invert()),
        )
        for (binarizer in binarizers) {
            val reader = MultiFormatReader().apply { setHints(hints) }
            try {
                val text = reader.decodeWithState(BinaryBitmap(binarizer)).text
                if (!text.isNullOrBlank()) return text
            } catch (_: NotFoundException) {
                // 换一种二值化继续
            } catch (_: Exception) {
                // 个别异常不影响后续尝试
            } finally {
                reader.reset()
            }
        }

        // 兜底：照片里二维码只占一小块时，多码模式更容易命中
        for (binarizer in listOf(
            HybridBinarizer(source),
            GlobalHistogramBinarizer(source),
        )) {
            val reader = GenericMultipleBarcodeReader(
                MultiFormatReader().apply { setHints(hints) },
            )
            try {
                val results = reader.decodeMultiple(BinaryBitmap(binarizer), hints)
                results.firstOrNull { !it.text.isNullOrBlank() }?.let { return it.text }
            } catch (_: NotFoundException) {
                // 继续下一种
            } catch (_: Exception) {
                // 忽略
            }
        }
        return null
    }
}
