#import <UIKit/UIKit.h>
#import <WebKit/WebKit.h>
#import <QuartzCore/QuartzCore.h>

// A dependency-free shell: one WKWebView pointed at the deployed web UI.
// Deliberately Objective-C, no Swift runtime, no frameworks, and built with an
// older toolchain so third-party signers (轻松签/Sideloadly) can re-sign it.
//
// The shell is responsible for everything the page cannot paint itself:
//   - the launch screen and a branded boot splash (never a white rectangle)
//   - a retry screen when the server is unreachable
//   - haptics requested from JS
//   - the status bar / keyboard appearance following the in-app theme

// MARK: - Palette

static UIColor *CCRBackgroundColor(void) {
  return [UIColor colorWithDynamicProvider:^UIColor *(UITraitCollection *trait) {
    if (trait.userInterfaceStyle == UIUserInterfaceStyleDark) {
      return [UIColor colorWithRed:15.0 / 255.0
                             green:16.0 / 255.0
                              blue:18.0 / 255.0
                             alpha:1.0];
    }
    return [UIColor colorWithRed:247.0 / 255.0
                           green:247.0 / 255.0
                            blue:248.0 / 255.0
                           alpha:1.0];
  }];
}

static UIColor *CCRPrimaryTextColor(void) {
  return [UIColor colorWithDynamicProvider:^UIColor *(UITraitCollection *trait) {
    if (trait.userInterfaceStyle == UIUserInterfaceStyleDark) {
      return [UIColor colorWithRed:242.0 / 255.0
                             green:243.0 / 255.0
                              blue:245.0 / 255.0
                             alpha:1.0];
    }
    return [UIColor colorWithRed:20.0 / 255.0
                           green:22.0 / 255.0
                            blue:26.0 / 255.0
                           alpha:1.0];
  }];
}

static UIColor *CCRMutedTextColor(void) {
  return [UIColor colorWithDynamicProvider:^UIColor *(UITraitCollection *trait) {
    if (trait.userInterfaceStyle == UIUserInterfaceStyleDark) {
      return [UIColor colorWithRed:111.0 / 255.0
                             green:118.0 / 255.0
                              blue:129.0 / 255.0
                             alpha:1.0];
    }
    return [UIColor colorWithRed:139.0 / 255.0
                           green:144.0 / 255.0
                            blue:154.0 / 255.0
                           alpha:1.0];
  }];
}

static UIColor *CCRBrandColor(void) {
  return [UIColor colorWithRed:217.0 / 255.0
                         green:119.0 / 255.0
                          blue:87.0 / 255.0
                         alpha:1.0];
}

/// Pauses gameplay-style animation for users who asked for less motion.
static BOOL CCRReduceMotion(void) {
  return UIAccessibilityIsReduceMotionEnabled();
}

// MARK: - Branded boot splash

/// Shown from the first frame until the page finishes loading. Uses the same
/// mark, wordmark and dots as the web boot splash so the hand-off is invisible.
@interface SplashView : UIView
@end

@interface SplashView ()
@property(nonatomic, strong) UIImageView *mark;
@property(nonatomic, strong) UILabel *wordmark;
@property(nonatomic, strong) NSArray<UIView *> *dots;
@property(nonatomic, assign) BOOL dismissed;
@end

@implementation SplashView

- (instancetype)initWithFrame:(CGRect)frame {
  self = [super initWithFrame:frame];
  if (!self) {
    return nil;
  }
  self.autoresizingMask =
      UIViewAutoresizingFlexibleWidth | UIViewAutoresizingFlexibleHeight;
  self.backgroundColor = CCRBackgroundColor();
  self.isAccessibilityElement = YES;
  self.accessibilityLabel = @"Loading Claude Remote";

  _mark = [[UIImageView alloc] initWithImage:[UIImage imageNamed:@"LaunchMark"]];
  _mark.contentMode = UIViewContentModeScaleAspectFit;
  _mark.isAccessibilityElement = NO;
  [self addSubview:_mark];

  _wordmark = [[UILabel alloc] initWithFrame:CGRectZero];
  _wordmark.attributedText = [[NSAttributedString alloc]
      initWithString:@"CLAUDE REMOTE"
          attributes:@{
            NSFontAttributeName :
                [UIFont systemFontOfSize:12 weight:UIFontWeightSemibold],
            NSKernAttributeName : @2.6,
            NSForegroundColorAttributeName : CCRMutedTextColor(),
          }];
  _wordmark.textAlignment = NSTextAlignmentCenter;
  _wordmark.isAccessibilityElement = NO;
  [self addSubview:_wordmark];

  NSMutableArray<UIView *> *dots = [NSMutableArray arrayWithCapacity:3];
  for (NSInteger i = 0; i < 3; i++) {
    UIView *dot = [[UIView alloc] initWithFrame:CGRectZero];
    dot.backgroundColor = CCRBrandColor();
    dot.layer.cornerRadius = 3.0;
    [self addSubview:dot];
    [dots addObject:dot];
  }
  _dots = dots;
  return self;
}

- (void)layoutSubviews {
  [super layoutSubviews];
  CGFloat width = CGRectGetWidth(self.bounds);
  CGFloat middle = CGRectGetMidY(self.bounds);

  // The mark keeps the launch screen's exact position (dead centre), so the
  // launch image and this view line up pixel-for-pixel.
  _mark.frame = CGRectMake(roundf((width - 96.0) / 2.0),
                           roundf(middle - 48.0), 96.0, 96.0);

  [_wordmark sizeToFit];
  CGSize wordSize = _wordmark.frame.size;
  _wordmark.frame = CGRectMake(roundf((width - wordSize.width) / 2.0),
                               roundf(middle + 64.0), ceilf(wordSize.width),
                               ceilf(wordSize.height));

  CGFloat dotSide = 6.0;
  CGFloat gap = 12.0;
  CGFloat total = dotSide * 3.0 + gap * 2.0;
  CGFloat x = roundf((width - total) / 2.0);
  for (UIView *dot in _dots) {
    dot.frame = CGRectMake(x, roundf(middle + 96.0), dotSide, dotSide);
    dot.layer.cornerRadius = dotSide / 2.0;
    x += dotSide + gap;
  }
}

- (void)playEntrance {
  _mark.alpha = 0.0;
  _mark.transform = CGAffineTransformMakeScale(0.9, 0.9);
  _wordmark.alpha = 0.0;
  for (UIView *dot in _dots) {
    dot.alpha = 0.2;
  }

  [UIView animateWithDuration:CCRReduceMotion() ? 0.0 : 0.55
                        delay:0.04
       usingSpringWithDamping:0.82
        initialSpringVelocity:0.4
                      options:UIViewAnimationOptionAllowUserInteraction
                   animations:^{
                     self.mark.alpha = 1.0;
                     self.mark.transform = CGAffineTransformIdentity;
                   }
                   completion:nil];
  [UIView animateWithDuration:CCRReduceMotion() ? 0.0 : 0.4
                        delay:0.18
                      options:UIViewAnimationOptionAllowUserInteraction
                   animations:^{
                     self.wordmark.alpha = 1.0;
                   }
                   completion:nil];

  if (CCRReduceMotion()) {
    return;
  }
  // A slow breath keeps the mark alive on a slow network without a spinner.
  [UIView animateWithDuration:1.8
                        delay:0.7
                      options:UIViewAnimationOptionAutoreverse |
                              UIViewAnimationOptionRepeat |
                              UIViewAnimationOptionCurveEaseInOut |
                              UIViewAnimationOptionAllowUserInteraction
                   animations:^{
                     self.mark.transform =
                         CGAffineTransformMakeScale(1.045, 1.045);
                   }
                   completion:nil];

  NSArray<NSNumber *> *delays = @[ @0.0, @0.15, @0.30 ];
  [self.dots enumerateObjectsUsingBlock:^(UIView *dot, NSUInteger index,
                                          BOOL *stop) {
    [UIView animateWithDuration:1.1
        delay:delays[index].doubleValue
        options:UIViewAnimationOptionAutoreverse |
                UIViewAnimationOptionRepeat |
                UIViewAnimationOptionCurveEaseInOut |
                UIViewAnimationOptionAllowUserInteraction
        animations:^{
          dot.alpha = 1.0;
          dot.transform = CGAffineTransformMakeTranslation(0, -3);
        }
        completion:nil];
  }];
}

- (void)dismiss:(void (^)(void))completion {
  if (_dismissed) {
    return;
  }
  _dismissed = YES;
  [_mark.layer removeAllAnimations];
  [_wordmark.layer removeAllAnimations];
  for (UIView *dot in _dots) {
    [dot.layer removeAllAnimations];
  }
  [UIView animateWithDuration:CCRReduceMotion() ? 0.0 : 0.32
                        delay:0
                      options:UIViewAnimationOptionCurveEaseOut |
                              UIViewAnimationOptionBeginFromCurrentState
                   animations:^{
                     self.alpha = 0.0;
                     // Animate the looping views back to rest inside the same
                     // fade so removing their animations is not a visible snap.
                     self.mark.transform = CGAffineTransformIdentity;
                     for (UIView *dot in self.dots) {
                       dot.alpha = 0.2;
                       dot.transform = CGAffineTransformIdentity;
                     }
                   }
                   completion:^(BOOL finished) {
                     [self removeFromSuperview];
                     if (completion) {
                       completion();
                     }
                   }];
}

@end

// MARK: - Offline / error screen

@interface ConnectionErrorView : UIView
@property(nonatomic, copy) void (^retryHandler)(void);
- (void)setDetailText:(NSString *)detail;
@end

@interface ConnectionErrorView ()
@property(nonatomic, strong) UIImageView *icon;
@property(nonatomic, strong) UILabel *title;
@property(nonatomic, strong) UILabel *detailLabel;
@property(nonatomic, strong) UIButton *retry;
@end

@implementation ConnectionErrorView

- (instancetype)initWithFrame:(CGRect)frame {
  self = [super initWithFrame:frame];
  if (!self) {
    return nil;
  }
  self.autoresizingMask =
      UIViewAutoresizingFlexibleWidth | UIViewAutoresizingFlexibleHeight;
  self.backgroundColor = CCRBackgroundColor();

  // `wifi.exclamationmark` shipped with iOS 14, matching the deployment target.
  _icon = [[UIImageView alloc] initWithImage:[UIImage systemImageNamed:
                                                  @"wifi.exclamationmark"]];
  _icon.tintColor = CCRMutedTextColor();
  _icon.contentMode = UIViewContentModeScaleAspectFit;
  [self addSubview:_icon];

  _title = [[UILabel alloc] initWithFrame:CGRectZero];
  _title.text = @"Can't reach the server";
  _title.font = [UIFont systemFontOfSize:20 weight:UIFontWeightSemibold];
  _title.textColor = CCRPrimaryTextColor();
  _title.textAlignment = NSTextAlignmentCenter;
  [self addSubview:_title];

  _detailLabel = [[UILabel alloc] initWithFrame:CGRectZero];
  _detailLabel.font = [UIFont systemFontOfSize:14 weight:UIFontWeightRegular];
  _detailLabel.textColor = CCRMutedTextColor();
  _detailLabel.textAlignment = NSTextAlignmentCenter;
  _detailLabel.numberOfLines = 0;
  [self addSubview:_detailLabel];

  UIButton *retry = [UIButton buttonWithType:UIButtonTypeSystem];
  [retry setTitle:@"Try Again" forState:UIControlStateNormal];
  [retry setTitleColor:[UIColor whiteColor] forState:UIControlStateNormal];
  retry.titleLabel.font = [UIFont systemFontOfSize:15 weight:UIFontWeightSemibold];
  retry.backgroundColor = CCRBrandColor();
  retry.layer.cornerRadius = 22.0;
  [retry addTarget:self
                action:@selector(retryTapped)
      forControlEvents:UIControlEventTouchUpInside];
  [self addSubview:retry];
  _retry = retry;
  return self;
}

- (void)setDetailText:(NSString *)detail {
  _detailLabel.text = detail.length > 0
                          ? detail
                          : @"Check your network connection and try again.";
}

- (void)layoutSubviews {
  [super layoutSubviews];
  CGFloat width = CGRectGetWidth(self.bounds);
  CGFloat middle = CGRectGetMidY(self.bounds);

  _icon.frame = CGRectMake(roundf((width - 34.0) / 2.0),
                           roundf(middle - 122.0), 34.0, 34.0);
  _title.frame = CGRectMake(24.0, roundf(middle - 70.0), width - 48.0, 26.0);
  _detailLabel.frame =
      CGRectMake(32.0, roundf(middle - 32.0), width - 64.0, 56.0);
  _retry.frame = CGRectMake(roundf((width - 148.0) / 2.0),
                            roundf(middle + 44.0), 148.0, 44.0);
}

- (void)retryTapped {
  if (self.retryHandler) {
    self.retryHandler();
  }
}

@end

// MARK: - JS bridge

/// JS -> native messages:
///   window.webkit.messageHandlers.haptic.postMessage('light'|'medium'|...)
///   window.webkit.messageHandlers.appearance.postMessage('light'|'dark'|'system')
static NSString *const CCRAppearanceDefaultsKey = @"CCRAppearance";
static NSString *const CCRAppearanceChangedNotification =
    @"CCRAppearanceChangedNotification";

@interface BridgeHandler : NSObject <WKScriptMessageHandler>
@end

@implementation BridgeHandler

- (void)userContentController:(WKUserContentController *)userContentController
      didReceiveScriptMessage:(WKScriptMessage *)message {
  if ([message.name isEqualToString:@"appearance"]) {
    NSString *mode = [message.body isKindOfClass:[NSString class]]
                         ? (NSString *)message.body
                         : @"system";
    if (![mode isEqualToString:@"light"] && ![mode isEqualToString:@"dark"] &&
        ![mode isEqualToString:@"system"]) {
      mode = @"system";
    }
    // Persist so the launch screen and splash already match the page theme.
    [[NSUserDefaults standardUserDefaults] setObject:mode
                                              forKey:CCRAppearanceDefaultsKey];
    dispatch_async(dispatch_get_main_queue(), ^{
      [[NSNotificationCenter defaultCenter]
          postNotificationName:CCRAppearanceChangedNotification
                        object:mode];
    });
    return;
  }

  NSString *kind = [message.body isKindOfClass:[NSString class]]
                       ? (NSString *)message.body
                       : @"light";
  dispatch_async(dispatch_get_main_queue(), ^{
    if ([kind isEqualToString:@"success"] ||
        [kind isEqualToString:@"warning"] ||
        [kind isEqualToString:@"error"]) {
      UINotificationFeedbackGenerator *generator =
          [[UINotificationFeedbackGenerator alloc] init];
      [generator prepare];
      UINotificationFeedbackType type = UINotificationFeedbackTypeSuccess;
      if ([kind isEqualToString:@"warning"]) {
        type = UINotificationFeedbackTypeWarning;
      } else if ([kind isEqualToString:@"error"]) {
        type = UINotificationFeedbackTypeError;
      }
      [generator notificationOccurred:type];
      return;
    }
    UIImpactFeedbackStyle style = [kind isEqualToString:@"medium"]
                                      ? UIImpactFeedbackStyleMedium
                                      : UIImpactFeedbackStyleLight;
    UIImpactFeedbackGenerator *generator =
        [[UIImpactFeedbackGenerator alloc] initWithStyle:style];
    [generator prepare];
    [generator impactOccurred];
  });
}

@end

// MARK: - Root controller

// Keeps the status bar legible against the dynamic page background.
@interface RootViewController : UIViewController
@end

@implementation RootViewController

- (UIStatusBarStyle)preferredStatusBarStyle {
  if (@available(iOS 13.0, *)) {
    return self.traitCollection.userInterfaceStyle == UIUserInterfaceStyleDark
               ? UIStatusBarStyleLightContent
               : UIStatusBarStyleDarkContent;
  }
  return UIStatusBarStyleDefault;
}

- (void)traitCollectionDidChange:(UITraitCollection *)previousTraitCollection {
  [super traitCollectionDidChange:previousTraitCollection];
  [self setNeedsStatusBarAppearanceUpdate];
}

@end

// MARK: - App delegate

@interface AppDelegate : UIResponder <UIApplicationDelegate,
                                      WKNavigationDelegate,
                                      WKUIDelegate>
@property(strong, nonatomic) UIWindow *window;
@property(strong, nonatomic) WKWebView *web;
@property(strong, nonatomic) SplashView *splash;
@property(strong, nonatomic) ConnectionErrorView *errorView;
@property(strong, nonatomic) NSDate *loadStartedAt;
@end

@implementation AppDelegate

- (BOOL)application:(UIApplication *)application
    didFinishLaunchingWithOptions:(NSDictionary *)launchOptions {
  self.window = [[UIWindow alloc] initWithFrame:[UIScreen mainScreen].bounds];

  // Restore the page's appearance choice before anything is drawn, so the
  // launch screen, splash and status bar never flash the wrong theme.
  NSString *appearance =
      [[NSUserDefaults standardUserDefaults] stringForKey:CCRAppearanceDefaultsKey];
  [self applyAppearance:appearance ?: @"system"];

  WKWebViewConfiguration *configuration = [[WKWebViewConfiguration alloc] init];
  static BridgeHandler *bridge;
  bridge = [[BridgeHandler alloc] init];
  [configuration.userContentController addScriptMessageHandler:bridge
                                                         name:@"haptic"];
  [configuration.userContentController addScriptMessageHandler:bridge
                                                         name:@"appearance"];
  configuration.allowsInlineMediaPlayback = YES;
  configuration.mediaTypesRequiringUserActionForPlayback =
      WKAudiovisualMediaTypeNone;

  WKWebView *web = [[WKWebView alloc] initWithFrame:self.window.bounds
                                     configuration:configuration];
  web.autoresizingMask =
      UIViewAutoresizingFlexibleWidth | UIViewAutoresizingFlexibleHeight;
  web.allowsBackForwardNavigationGestures = YES;
  web.navigationDelegate = self;
  // Without a WKUIDelegate, window.confirm() returns false and
  // window.alert() does nothing — the "restart terminal" confirmation in the
  // page looked like a dead button.
  web.UIDelegate = self;
  // The page owns safe-area insets and the 100dvh layout.
  web.scrollView.contentInsetAdjustmentBehavior =
      UIScrollViewContentInsetAdjustmentNever;
  // The document never scrolls; only inner views do. Turning off bounce keeps
  // a drag at the end of a list from rubber-banding the whole page.
  web.scrollView.bounces = NO;
  web.scrollView.alwaysBounceVertical = NO;
  web.scrollView.alwaysBounceHorizontal = NO;
  UIColor *background = CCRBackgroundColor();
  // The keyboard shrinks the web view; whatever is behind it must be the page
  // background, not the default black window.
  web.opaque = YES;
  web.backgroundColor = background;
  web.scrollView.backgroundColor = background;
  web.scrollView.keyboardDismissMode = UIScrollViewKeyboardDismissModeInteractive;
  web.scrollView.automaticallyAdjustsScrollIndicatorInsets = NO;
  self.window.backgroundColor = background;
  RootViewController *controller = [[RootViewController alloc] init];
  controller.view.backgroundColor = background;
  controller.view = web;
  self.window.rootViewController = controller;
  [self.window makeKeyAndVisible];
  self.web = web;

  [[NSNotificationCenter defaultCenter]
      addObserver:self
         selector:@selector(appearanceDidChange:)
             name:CCRAppearanceChangedNotification
           object:nil];

  [self showSplash];
  [self loadApp];
  return YES;
}

#pragma mark - Appearance

- (void)applyAppearance:(NSString *)mode {
  UIUserInterfaceStyle style = UIUserInterfaceStyleUnspecified;
  if ([mode isEqualToString:@"dark"]) {
    style = UIUserInterfaceStyleDark;
  } else if ([mode isEqualToString:@"light"]) {
    style = UIUserInterfaceStyleLight;
  }
  self.window.overrideUserInterfaceStyle = style;
  [self.window.rootViewController setNeedsStatusBarAppearanceUpdate];
}

- (void)appearanceDidChange:(NSNotification *)notification {
  [self applyAppearance:(NSString *)notification.object ?: @"system"];
}

#pragma mark - Loading

- (NSURLRequest *)appRequest {
  NSURL *url = [NSURL URLWithString:@"http://8.166.138.207/code/"];
  // Always revalidate the HTML; hashed assets are immutable. A cached
  // index.html is what makes a freshly deployed UI look unchanged.
  return [NSURLRequest requestWithURL:url
                          cachePolicy:NSURLRequestReloadIgnoringLocalCacheData
                      timeoutInterval:30];
}

- (void)loadApp {
  self.loadStartedAt = [NSDate date];
  [self.web loadRequest:[self appRequest]];
}

- (void)showSplash {
  if (self.splash) {
    return;
  }
  SplashView *splash = [[SplashView alloc] initWithFrame:self.window.bounds];
  [self.window addSubview:splash];
  self.splash = splash;
  [splash playEntrance];
}

- (void)hideSplash {
  SplashView *splash = self.splash;
  if (!splash) {
    return;
  }
  self.splash = nil;
  [splash dismiss:nil];
}

- (void)showError:(NSError *)error {
  [self hideSplash];
  if (self.errorView) {
    [self.errorView setDetailText:error.localizedDescription];
    return;
  }
  ConnectionErrorView *view =
      [[ConnectionErrorView alloc] initWithFrame:self.window.bounds];
  [view setDetailText:error.localizedDescription];
  __weak typeof(self) weakSelf = self;
  view.retryHandler = ^{
    [weakSelf retry];
  };
  [self.window addSubview:view];
  self.errorView = view;

  view.alpha = 0.0;
  [UIView animateWithDuration:0.22
                   animations:^{
                     view.alpha = 1.0;
                   }];
}

- (void)hideError {
  ConnectionErrorView *view = self.errorView;
  if (!view) {
    return;
  }
  self.errorView = nil;
  [UIView animateWithDuration:0.18
      animations:^{
        view.alpha = 0.0;
      }
      completion:^(BOOL finished) {
        [view removeFromSuperview];
      }];
}

- (void)retry {
  [self hideError];
  [self showSplash];
  [self loadApp];
}

#pragma mark - WKNavigationDelegate

- (void)webView:(WKWebView *)webView
    didStartProvisionalNavigation:(WKNavigation *)navigation {
  [self hideError];
}

- (void)webView:(WKWebView *)webView
    didFinishNavigation:(WKNavigation *)navigation {
  [self hideError];
  // Keep the splash up for a beat even on a warm cache so the mark does not
  // blink, then hand over to the page (whose own boot splash shares the mark).
  NSTimeInterval elapsed = -[self.loadStartedAt timeIntervalSinceNow];
  NSTimeInterval delay = MAX(0.0, 0.75 - elapsed) + 0.05;
  __weak typeof(self) weakSelf = self;
  dispatch_after(dispatch_time(DISPATCH_TIME_NOW, (int64_t)(delay * NSEC_PER_SEC)),
                 dispatch_get_main_queue(), ^{
                   [weakSelf hideSplash];
                 });
}

- (void)webView:(WKWebView *)webView
    didFailProvisionalNavigation:(WKNavigation *)navigation
                       withError:(NSError *)error {
  if (error.code == NSURLErrorCancelled) {
    return;
  }
  [self showError:error];
}

- (void)webView:(WKWebView *)webView
    didFailNavigation:(WKNavigation *)navigation
            withError:(NSError *)error {
  if (error.code == NSURLErrorCancelled) {
    return;
  }
  [self showError:error];
}

#pragma mark - WKUIDelegate (JS dialogs)

- (void)presentAlert:(UIAlertController *)alert completion:(void (^)(void))done {
  UIViewController *root = self.window.rootViewController;
  if (!root) {
    // Never leave the JS completion handler hanging.
    done();
    return;
  }
  UIViewController *top = root;
  while (top.presentedViewController) {
    top = top.presentedViewController;
  }
  [top presentViewController:alert animated:YES completion:nil];
}

- (void)webView:(WKWebView *)webView
    runJavaScriptAlertPanelWithMessage:(NSString *)message
                      initiatedByFrame:(WKFrameInfo *)frame
                     completionHandler:(void (^)(void))completionHandler {
  UIAlertController *alert =
      [UIAlertController alertControllerWithTitle:nil
                                          message:message
                                   preferredStyle:UIAlertControllerStyleAlert];
  __block BOOL done = NO;
  [alert addAction:[UIAlertAction actionWithTitle:@"好"
                                            style:UIAlertActionStyleDefault
                                          handler:^(UIAlertAction *action) {
                                            if (done) return;
                                            done = YES;
                                            completionHandler();
                                          }]];
  dispatch_async(dispatch_get_main_queue(), ^{
    [self presentAlert:alert
            completion:^{
              if (done) return;
              done = YES;
              completionHandler();
            }];
  });
}

- (void)webView:(WKWebView *)webView
    runJavaScriptConfirmPanelWithMessage:(NSString *)message
                        initiatedByFrame:(WKFrameInfo *)frame
                       completionHandler:(void (^)(BOOL))completionHandler {
  UIAlertController *alert =
      [UIAlertController alertControllerWithTitle:nil
                                          message:message
                                   preferredStyle:UIAlertControllerStyleAlert];
  __block BOOL done = NO;
  [alert addAction:[UIAlertAction actionWithTitle:@"取消"
                                            style:UIAlertActionStyleCancel
                                          handler:^(UIAlertAction *action) {
                                            if (done) return;
                                            done = YES;
                                            completionHandler(NO);
                                          }]];
  [alert addAction:[UIAlertAction actionWithTitle:@"确定"
                                            style:UIAlertActionStyleDefault
                                          handler:^(UIAlertAction *action) {
                                            if (done) return;
                                            done = YES;
                                            completionHandler(YES);
                                          }]];
  dispatch_async(dispatch_get_main_queue(), ^{
    [self presentAlert:alert
            completion:^{
              if (done) return;
              done = YES;
              completionHandler(NO);
            }];
  });
}

- (void)webView:(WKWebView *)webView
    runJavaScriptTextInputPanelWithPrompt:(NSString *)prompt
                              defaultText:(NSString *)defaultText
                         initiatedByFrame:(WKFrameInfo *)frame
                        completionHandler:(void (^)(NSString *_Nullable))completionHandler {
  UIAlertController *alert =
      [UIAlertController alertControllerWithTitle:nil
                                          message:prompt
                                   preferredStyle:UIAlertControllerStyleAlert];
  [alert addTextFieldWithConfigurationHandler:^(UITextField *textField) {
    textField.text = defaultText;
  }];
  __block BOOL done = NO;
  [alert addAction:[UIAlertAction actionWithTitle:@"取消"
                                            style:UIAlertActionStyleCancel
                                          handler:^(UIAlertAction *action) {
                                            if (done) return;
                                            done = YES;
                                            completionHandler(nil);
                                          }]];
  [alert addAction:[UIAlertAction actionWithTitle:@"确定"
                                            style:UIAlertActionStyleDefault
                                          handler:^(UIAlertAction *action) {
                                            if (done) return;
                                            done = YES;
                                            completionHandler(
                                                alert.textFields.firstObject.text);
                                          }]];
  dispatch_async(dispatch_get_main_queue(), ^{
    [self presentAlert:alert
            completion:^{
              if (done) return;
              done = YES;
              completionHandler(defaultText ?: @"");
            }];
  });
}

- (void)webViewWebContentProcessDidTerminate:(WKWebView *)webView {
  // iOS killed the web content process (usually memory pressure). A reload is
  // the only recovery, and it beats leaving the user on a blank page.
  [self showSplash];
  [self loadApp];
}

@end

int main(int argc, char *argv[]) {
  @autoreleasepool {
    return UIApplicationMain(argc, argv, nil,
                             NSStringFromClass([AppDelegate class]));
  }
}
