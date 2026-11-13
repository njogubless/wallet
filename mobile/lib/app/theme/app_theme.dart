import 'package:flutter/material.dart';

import 'tokens.dart';

const _family = 'PlusJakartaSans';

/// Plus Jakarta Sans is bundled as a variable font; weight is applied through
/// the `wght` axis as well as fontWeight.
TextStyle jakarta(double size, FontWeight weight, {double? height, double? spacing, Color? color}) => TextStyle(
      fontFamily: _family,
      fontSize: size,
      fontWeight: weight,
      fontVariations: [FontVariation('wght', weight.value.toDouble())],
      height: height == null ? null : height / size,
      letterSpacing: spacing == null ? null : spacing * size,
      color: color,
    );

/// Tabular figures for every amount so columns align.
const tabular = [FontFeature.tabularFigures()];

abstract final class AppText {
  static final displayLg = jakarta(40, FontWeight.w700, height: 48, spacing: -0.02);
  static final displayMobile = jakarta(32, FontWeight.w700, height: 40, spacing: -0.02);
  static final headlineLg = jakarta(28, FontWeight.w700, height: 36, spacing: -0.01);
  static final headlineMd = jakarta(22, FontWeight.w600, height: 28, spacing: -0.01);
  static final headlineSm = jakarta(18, FontWeight.w600, height: 24);
  static final titleMd = jakarta(16, FontWeight.w600, height: 22);
  static final bodyLg = jakarta(16, FontWeight.w400, height: 24);
  static final bodyMd = jakarta(14, FontWeight.w400, height: 20);
  static final bodySm = jakarta(12, FontWeight.w400, height: 16);
  static final labelLg = jakarta(14, FontWeight.w600, height: 20, spacing: 0.01);
  static final labelMd = jakarta(12, FontWeight.w600, height: 16, spacing: 0.02);
  static final labelSm = jakarta(11, FontWeight.w700, height: 14, spacing: 0.04);
  static final currencyPrefix = jakarta(14, FontWeight.w500, height: 20, spacing: 0.05, color: AppColors.muted);
}

ThemeData buildTheme() {
  final scheme = ColorScheme.fromSeed(
    seedColor: AppColors.primary,
    primary: AppColors.primary,
    onPrimary: Colors.white,
    secondary: AppColors.secondary,
    surface: AppColors.surface,
    onSurface: AppColors.onSurface,
    error: AppColors.danger,
  );
  final controlShape = RoundedRectangleBorder(borderRadius: BorderRadius.circular(AppRadius.control));
  return ThemeData(
    useMaterial3: true,
    colorScheme: scheme,
    fontFamily: _family,
    scaffoldBackgroundColor: AppColors.canvas,
    textTheme: TextTheme(
      displayLarge: AppText.displayLg,
      displayMedium: AppText.displayMobile,
      headlineLarge: AppText.headlineLg,
      headlineMedium: AppText.headlineMd,
      headlineSmall: AppText.headlineSm,
      titleMedium: AppText.titleMd,
      bodyLarge: AppText.bodyLg,
      bodyMedium: AppText.bodyMd,
      bodySmall: AppText.bodySm,
      labelLarge: AppText.labelLg,
      labelMedium: AppText.labelMd,
      labelSmall: AppText.labelSm,
    ).apply(bodyColor: AppColors.onSurface, displayColor: AppColors.onSurface),
    appBarTheme: AppBarTheme(
      backgroundColor: AppColors.surface,
      foregroundColor: AppColors.onSurface,
      elevation: 0,
      scrolledUnderElevation: 0.5,
      titleTextStyle: AppText.headlineSm.copyWith(color: AppColors.onSurface),
    ),
    filledButtonTheme: FilledButtonThemeData(
      style: FilledButton.styleFrom(
        backgroundColor: AppColors.primary,
        foregroundColor: Colors.white,
        minimumSize: const Size.fromHeight(52),
        shape: controlShape,
        textStyle: AppText.labelLg,
      ),
    ),
    outlinedButtonTheme: OutlinedButtonThemeData(
      style: OutlinedButton.styleFrom(
        foregroundColor: AppColors.primary,
        minimumSize: const Size.fromHeight(48),
        side: const BorderSide(color: AppColors.primary),
        shape: controlShape,
        textStyle: AppText.labelLg,
      ),
    ),
    textButtonTheme: TextButtonThemeData(
      style: TextButton.styleFrom(foregroundColor: AppColors.primary, textStyle: AppText.labelLg, minimumSize: const Size(48, 48)),
    ),
    inputDecorationTheme: InputDecorationTheme(
      filled: true,
      fillColor: AppColors.surface,
      contentPadding: const EdgeInsets.symmetric(horizontal: 16, vertical: 14),
      labelStyle: AppText.bodyMd.copyWith(color: AppColors.muted),
      floatingLabelStyle: AppText.labelMd.copyWith(color: AppColors.primary),
      border: OutlineInputBorder(borderRadius: BorderRadius.circular(AppRadius.control), borderSide: const BorderSide(color: AppColors.border)),
      enabledBorder: OutlineInputBorder(borderRadius: BorderRadius.circular(AppRadius.control), borderSide: const BorderSide(color: AppColors.border)),
      focusedBorder: OutlineInputBorder(borderRadius: BorderRadius.circular(AppRadius.control), borderSide: const BorderSide(color: AppColors.primary, width: 2)),
      errorBorder: OutlineInputBorder(borderRadius: BorderRadius.circular(AppRadius.control), borderSide: const BorderSide(color: AppColors.danger)),
    ),
    cardTheme: CardThemeData(
      color: AppColors.surface,
      elevation: 0,
      margin: EdgeInsets.zero,
      shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(AppRadius.card), side: const BorderSide(color: AppColors.border)),
    ),
    chipTheme: ChipThemeData(
      backgroundColor: AppColors.surface,
      selectedColor: AppColors.primary,
      labelStyle: AppText.labelMd,
      side: const BorderSide(color: AppColors.border),
      shape: const StadiumBorder(),
    ),
    dividerTheme: const DividerThemeData(color: AppColors.border, thickness: 1, space: 1),
    snackBarTheme: SnackBarThemeData(behavior: SnackBarBehavior.floating, backgroundColor: AppColors.primary, contentTextStyle: AppText.bodyMd.copyWith(color: Colors.white)),
  );
}
