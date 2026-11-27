import java.util.Properties

plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

// release 签名配置从项目根目录（client-android/）的 keystore.properties 读取。
// 该文件不入库，格式见 keystore.properties.example；不存在时不启用 release 签名。
val keystorePropertiesFile = rootProject.file("keystore.properties")
val keystoreProperties = Properties().apply {
    if (keystorePropertiesFile.exists()) {
        keystorePropertiesFile.inputStream().use { load(it) }
    }
}

fun keystoreProperty(name: String): String =
    keystoreProperties.getProperty(name)
        ?: throw GradleException("keystore.properties 缺少 $name，请参考 keystore.properties.example")

android {
    namespace = "io.homing.client"
    compileSdk = 34

    defaultConfig {
        applicationId = "io.homing.client"
        minSdk = 24
        targetSdk = 34
        versionCode = 33
        versionName = "2.4.1"
    }
    signingConfigs {
        // 只在配置存在时创建 release 签名；没有 keystore.properties 时 release 不签名。
        if (keystorePropertiesFile.exists()) {
            create("release") {
                // storeFile 相对 client-android/ 目录，也可写绝对路径
                storeFile = rootProject.file(keystoreProperty("storeFile"))
                storePassword = keystoreProperty("storePassword")
                keyAlias = keystoreProperty("keyAlias")
                keyPassword = keystoreProperty("keyPassword")
            }
        }
    }
    buildTypes {
        debug {
            // 使用 Android 默认 debug 签名（~/.android/debug.keystore），无需仓库内 keystore
        }
        release {
            isMinifyEnabled = false
            isShrinkResources = false
            // 有 keystore.properties 才签名，否则产出未签名 APK（方便无签名材料的贡献者只验证编译）
            if (keystorePropertiesFile.exists()) {
                signingConfig = signingConfigs.getByName("release")
            }
            // 渠道/安装包标识（可选）
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
        }
    }
    buildFeatures {
        buildConfig = true
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions { jvmTarget = "17" }
}

dependencies {
    implementation("androidx.appcompat:appcompat:1.7.0")
    implementation("androidx.core:core-ktx:1.13.1")
    implementation("com.google.android.material:material:1.12.0")
    implementation("androidx.recyclerview:recyclerview:1.3.2")
    implementation("androidx.core:core-ktx:1.13.1")
    implementation("com.wireguard.android:tunnel:1.0.20230706")
    // 扫码导入：ZXing 封装（自带 CaptureActivity，运行时申请相机权限）
    implementation("com.journeyapps:zxing-android-embedded:4.3.0")
}
