cmake_minimum_required(VERSION 3.20)
project(onedriver-dolphin LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

find_package(Qt6 6.5 REQUIRED COMPONENTS Core Gui Widgets DBus Test)
find_package(ECM REQUIRED NO_MODULE)
set(CMAKE_MODULE_PATH ${ECM_MODULE_PATH})

include(KDEInstallDirs6)
include(KDECMakeSettings)

find_package(KF6 REQUIRED COMPONENTS CoreAddons I18n KIO)

set(CMAKE_AUTOMOC ON)
add_definitions(-DTRANSLATION_DOMAIN="onedriver-dolphin")

# pure logic (state -> emblem, mount matching, xattr access), shared with the tests
add_library(onedriver-dolphin-core STATIC src/logic.cpp src/logic.h)
target_link_libraries(onedriver-dolphin-core PUBLIC Qt6::Core)
set_target_properties(onedriver-dolphin-core PROPERTIES POSITION_INDEPENDENT_CODE ON)

# kf6/overlayicon/onedriveroverlay.so
add_library(onedriveroverlay MODULE src/overlayplugin.cpp src/overlayplugin.h)
target_link_libraries(onedriveroverlay onedriver-dolphin-core Qt6::DBus KF6::KIOCore KF6::CoreAddons)
install(TARGETS onedriveroverlay DESTINATION ${KDE_INSTALL_PLUGINDIR}/kf6/overlayicon)

# kf6/kfileitemaction/onedriveraction.so
add_library(onedriveraction MODULE src/actionplugin.cpp src/actionplugin.h)
target_link_libraries(onedriveraction onedriver-dolphin-core Qt6::DBus Qt6::Widgets
    KF6::KIOCore KF6::KIOWidgets KF6::CoreAddons KF6::I18n)
install(TARGETS onedriveraction DESTINATION ${KDE_INSTALL_PLUGINDIR}/kf6/kfileitemaction)

# emblems: cloud (not on this device), local, pinned; Breeze has no cloud
# emblem and its check marks follow the theme color, so local and pinned
# would look the same
install(FILES icons/onedriver-emblem-cloud.svg icons/onedriver-emblem-local.svg icons/onedriver-emblem-pinned.svg
        DESTINATION ${KDE_INSTALL_ICONDIR}/hicolor/scalable/emblems)

# translations: msgfmt each po/<lang>.po into the onedriver-dolphin domain
find_program(MSGFMT msgfmt)
file(GLOB PO_FILES ${CMAKE_CURRENT_SOURCE_DIR}/po/*.po)
if(MSGFMT)
    foreach(po ${PO_FILES})
        get_filename_component(lang ${po} NAME_WE)
        set(mo ${CMAKE_CURRENT_BINARY_DIR}/${lang}.mo)
        add_custom_command(OUTPUT ${mo} COMMAND ${MSGFMT} -o ${mo} ${po} DEPENDS ${po})
        add_custom_target(mo-${lang} ALL DEPENDS ${mo})
        install(FILES ${mo} DESTINATION ${KDE_INSTALL_LOCALEDIR}/${lang}/LC_MESSAGES
                RENAME onedriver-dolphin.mo)
    endforeach()
endif()

option(BUILD_TESTING "Build the tests" ON)
if(BUILD_TESTING)
    enable_testing()
    add_executable(tst_logic tests/tst_logic.cpp)
    target_link_libraries(tst_logic onedriver-dolphin-core Qt6::Test)
    add_test(NAME logic COMMAND tst_logic)

    # loads the real overlay plugin against a fake mount service on a private bus
    add_executable(smoke tests/smoke.cpp)
    target_link_libraries(smoke Qt6::DBus Qt6::Core KF6::KIOCore KF6::CoreAddons)
endif()
