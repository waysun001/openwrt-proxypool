/*
 * ip2region xdb v2.0 搜索器
 * 单文件、零外部依赖，适用于 OpenWrt 嵌入式环境
 *
 * 用法: ip2region_searcher search <xdb_file> <ip_address>
 * 输出: 中国|0|广东省|深圳市|电信
 */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>

/* xdb v2.0 格式常量 */
#define HEADER_SIZE         256
#define VECTOR_INDEX_OFFSET 256
#define VECTOR_INDEX_ROWS   256
#define VECTOR_INDEX_COLS   256
#define VECTOR_INDEX_SIZE   (VECTOR_INDEX_ROWS * VECTOR_INDEX_COLS * 8)
#define INDEX_ENTRY_SIZE    14  /* start_ip(4) + end_ip(4) + data_len(2) + data_ptr(4) */

/* 小端序读取 */
static uint32_t get_u32_le(const unsigned char *p)
{
    return (uint32_t)p[0]
         | ((uint32_t)p[1] << 8)
         | ((uint32_t)p[2] << 16)
         | ((uint32_t)p[3] << 24);
}

static uint16_t get_u16_le(const unsigned char *p)
{
    return (uint16_t)p[0] | ((uint16_t)p[1] << 8);
}

/* 将点分十进制 IP 转为网络序 uint32 (高位在前) */
static int parse_ip(const char *s, uint32_t *out)
{
    unsigned int a, b, c, d;

    if (sscanf(s, "%u.%u.%u.%u", &a, &b, &c, &d) != 4)
        return -1;
    if (a > 255 || b > 255 || c > 255 || d > 255)
        return -1;

    *out = (a << 24) | (b << 16) | (c << 8) | d;
    return 0;
}

/* 加载整个文件到内存 */
static unsigned char *load_file(const char *path, long *out_size)
{
    FILE *f = fopen(path, "rb");
    if (!f)
        return NULL;

    fseek(f, 0, SEEK_END);
    long size = ftell(f);
    fseek(f, 0, SEEK_SET);

    if (size < HEADER_SIZE + VECTOR_INDEX_SIZE) {
        fclose(f);
        return NULL;
    }

    unsigned char *buf = (unsigned char *)malloc((size_t)size);
    if (!buf) {
        fclose(f);
        return NULL;
    }

    if ((long)fread(buf, 1, (size_t)size, f) != size) {
        free(buf);
        fclose(f);
        return NULL;
    }

    fclose(f);
    *out_size = size;
    return buf;
}

/* 在索引段中二分查找 IP */
static int search_index(const unsigned char *buf, long file_size,
                        uint32_t start_ptr, uint32_t end_ptr, uint32_t ip)
{
    int lo = 0;
    int hi = (int)((end_ptr - start_ptr) / INDEX_ENTRY_SIZE);

    while (lo <= hi) {
        int mid = (lo + hi) >> 1;
        uint32_t off = start_ptr + (uint32_t)mid * INDEX_ENTRY_SIZE;

        uint32_t sip = get_u32_le(buf + off);
        uint32_t eip = get_u32_le(buf + off + 4);

        if (ip < sip) {
            hi = mid - 1;
        } else if (ip > eip) {
            lo = mid + 1;
        } else {
            /* 命中：读取数据 */
            uint16_t data_len = get_u16_le(buf + off + 8);
            uint32_t data_ptr = get_u32_le(buf + off + 10);

            if (data_ptr + data_len > (uint32_t)file_size)
                return -1;

            fwrite(buf + data_ptr, 1, data_len, stdout);
            putchar('\n');
            return 0;
        }
    }

    return -1;
}

static void usage(const char *prog)
{
    fprintf(stderr, "Usage: %s search <xdb_file> <ip_address>\n", prog);
}

int main(int argc, char *argv[])
{
    if (argc != 4 || strcmp(argv[1], "search") != 0) {
        usage(argv[0]);
        return 1;
    }

    const char *xdb_path = argv[2];
    const char *ip_str   = argv[3];

    /* 解析 IP */
    uint32_t ip;
    if (parse_ip(ip_str, &ip) != 0) {
        fprintf(stderr, "Error: invalid IP address '%s'\n", ip_str);
        return 1;
    }

    /* 加载 xdb 到内存 (content-based 模式) */
    long file_size = 0;
    unsigned char *buf = load_file(xdb_path, &file_size);
    if (!buf) {
        fprintf(stderr, "Error: cannot load xdb file '%s'\n", xdb_path);
        return 1;
    }

    /* 通过向量索引定位搜索范围 (第一字节 * 256 + 第二字节) */
    unsigned int i0 = (ip >> 24) & 0xFF;
    unsigned int i1 = (ip >> 16) & 0xFF;
    uint32_t vec_off = VECTOR_INDEX_OFFSET + (i0 * VECTOR_INDEX_COLS + i1) * 8;

    uint32_t start_ptr = get_u32_le(buf + vec_off);
    uint32_t end_ptr   = get_u32_le(buf + vec_off + 4);

    /* 二分查找 */
    int rc = search_index(buf, file_size, start_ptr, end_ptr, ip);

    free(buf);

    if (rc != 0) {
        fprintf(stderr, "Error: no record found for '%s'\n", ip_str);
        return 1;
    }

    return 0;
}
